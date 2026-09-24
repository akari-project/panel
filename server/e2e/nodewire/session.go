// SPDX-License-Identifier: AGPL-3.0-or-later

package nodewire

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/internal/clock"
)

// 待确认窗口的默认上限（NODE-14）。
const (
	DefaultWindowMessages = 1000
	DefaultWindowBytes    = 8 << 20
)

// ErrWindowFull 表示有界会话的待确认条目已达窗口上限（控制面 → 节点方向，NODE-14）。
var ErrWindowFull = errors.New("nodewire: send window full")

// ErrSessionClosed 表示会话已结束。
var ErrSessionClosed = errors.New("nodewire: session closed")

// Outgoing 是一条待确认的信封。Env 只需填写 idem_key 与 body；ack 与 ts_ms 在发送时填写。
// 会话结束后，未确认的条目可以原样交给新会话，以新的 seq 重发（NODE-12）。
type Outgoing struct {
	Env *nodev1.Envelope
	Tag any // 由调用方使用，例如流量报告的 report_seq
	// Key 非空时，队列中尚未发送的同 Key 条目被新条目替换（例如只保留最新的状态上报），
	// 使节点方向未发送的队列有上限（NODE-14）。
	Key string
	// Exempt 的条目不计入有界会话的窗口（Reset 放入的全量快照），避免快照接近窗口字节上限时
	// 每次发送都再次溢出。
	Exempt bool

	size int
	seq  uint64 // 本会话内的 seq；0 表示尚未发送
}

// NewOutgoing 包装一个信封。
func NewOutgoing(env *nodev1.Envelope, tag any) *Outgoing {
	return &Outgoing{Env: env, Tag: tag, size: proto.Size(env)}
}

func (o *Outgoing) clone() *Outgoing {
	return &Outgoing{Env: o.Env, Tag: o.Tag, Key: o.Key, Exempt: o.Exempt, size: o.size}
}

// SessionConfig 配置一个已完成握手的会话。
type SessionConfig struct {
	Conn    Conn
	SendKey []byte
	RecvKey []byte
	SendDir byte // 本端发送方向；接收方向取另一个
	Clock   clock.Clock
	// Rand 提供 nonce 与 ack_only 的 idem_key，默认 crypto/rand。必须并发安全：写协程与调用方可能同时读取；
	// 非 crypto/rand 的来源用 LockedReader 包装。
	Rand io.Reader

	WindowMessages int // 默认 DefaultWindowMessages
	WindowBytes    int // 默认 DefaultWindowBytes
	// Bounded 为 true 时，Send 在未确认条目（已发送与未发送之和）达到窗口上限时返回 ErrWindowFull，
	// 由调用方改发全量（控制面方向）。为 false 时只限制在途条目，其余排队等待（节点方向的背压）。
	Bounded bool

	Dedup  *Dedup    // 跨会话的 idem_key 去重记录；nil 表示不去重
	Faults FaultPlan // 可选

	// Handler 在读协程中按顺序收到每个首次出现的业务信封（不含 ack_only）。不得阻塞，
	// 可以调用本会话的 Send 与 Close。
	Handler func(env *nodev1.Envelope)
	// OnAcked 在读协程中收到被确认的条目。
	OnAcked func(acked []*Outgoing)
	// OnReceive 在读协程中收到每个通过认证的信封（包括 ack_only 与重复的信封），先于 Handler。
	// 控制面以此更新心跳（NODE-20）。
	OnReceive func(env *nodev1.Envelope)

	Initial []*Outgoing // 上一会话未确认的条目，先于新条目发送
}

// SessionStats 是会话的计数。
type SessionStats struct {
	FramesSent, FramesReceived uint64
	Duplicates                 uint64 // 按 idem_key 去重跳过的信封
	Unacked                    int
}

// Session 在一条已认证的连接上实现信封加密、seq 与 ack、窗口与重传队列（NODE-11–14）。
// 每个会话一个读协程、一个写协程；持锁期间不做 I/O。
type Session struct {
	cfg    SessionConfig
	sealer *Sealer
	opener *Opener

	ctx        context.Context
	cancel     context.CancelFunc
	wake       chan struct{}
	done       chan struct{}
	running    atomic.Int32
	stopParent func() bool
	graceTimer atomic.Pointer[time.Timer]

	mu            sync.Mutex
	queue         []*Outgoing // 已发送未确认的在前，未发送的在后
	nsent         int
	inflightBytes int
	queuedBytes   int
	sendSeq       uint64
	recvSeq       uint64
	needAck       bool
	epoch         uint64 // Reset 的次数
	exemptN       int    // 队列中 Exempt 条目的数量与字节数
	exemptBytes   int
	closeCode     int // 非 0 表示请求以此关闭码关闭
	closeReason   string
	err           error
	stats         SessionStats
}

// NewSession 创建会话；调用 Start 后开始收发。
func NewSession(cfg SessionConfig) (*Session, error) {
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	} else if cfg.Rand != rand.Reader {
		cfg.Rand = LockedReader(cfg.Rand)
	}
	if cfg.WindowMessages <= 0 {
		cfg.WindowMessages = DefaultWindowMessages
	}
	if cfg.WindowBytes <= 0 {
		cfg.WindowBytes = DefaultWindowBytes
	}
	recvDir := DirPanelToAgent
	if cfg.SendDir == DirPanelToAgent {
		recvDir = DirAgentToPanel
	}
	sealer, err := NewSealer(cfg.SendKey, cfg.SendDir, cfg.Rand)
	if err != nil {
		return nil, err
	}
	opener, err := NewOpener(cfg.RecvKey, recvDir)
	if err != nil {
		return nil, err
	}
	s := &Session{
		cfg:    cfg,
		sealer: sealer,
		opener: opener,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	// ctx 在构造时建立，Close 与 Abort 可以在 Start 之前调用（控制面先登记会话、写出 hello_ack 后才启动）。
	s.ctx, s.cancel = context.WithCancel(context.Background())
	for _, o := range cfg.Initial {
		s.queue = append(s.queue, o.clone())
		s.queuedBytes += o.size
	}
	return s, nil
}

// Start 启动读写协程。ctx 结束时会话中断。
func (s *Session) Start(ctx context.Context) {
	s.stopParent = context.AfterFunc(ctx, s.cancel)
	s.running.Store(2)
	go func() { defer s.exit(); s.readLoop() }()
	go func() { defer s.exit(); s.writeLoop() }()
}

// exit 在读写协程各自退出时调用。读协程因协议错误退出时不取消 ctx，留给写协程发送关闭码；
// 最后一个退出的协程确保底层连接已关闭，然后关闭 done。
func (s *Session) exit() {
	if s.running.Add(-1) == 0 {
		_ = s.cfg.Conn.Abort()
		s.cancel()
		s.stopParent()
		if t := s.graceTimer.Load(); t != nil {
			t.Stop()
		}
		close(s.done)
	}
}

// Done 在读写协程都退出后关闭。
func (s *Session) Done() <-chan struct{} { return s.done }

// Err 返回会话结束的原因（会话结束前可能为 nil）。
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Stats 返回计数快照。
func (s *Session) Stats() SessionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Unacked = len(s.queue)
	return st
}

// Send 把信封加入发送队列，不阻塞。
func (s *Session) Send(o *Outgoing) error {
	s.mu.Lock()
	if s.closeCode != 0 || s.err != nil {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	if o.Key != "" {
		for i := s.nsent; i < len(s.queue); i++ {
			if old := s.queue[i]; old.Key == o.Key && !old.Exempt {
				s.queue[i] = o
				s.queuedBytes += o.size - old.size
				s.mu.Unlock()
				s.signal()
				return nil
			}
		}
	}
	if !o.Exempt && !s.fitsLocked(o) {
		s.mu.Unlock()
		return ErrWindowFull
	}
	s.addLocked(o)
	s.mu.Unlock()
	s.signal()
	return nil
}

// fitsLocked 报告有界会话能否再容纳 o（不计 Exempt 条目）；无界会话总是可以。
func (s *Session) fitsLocked(o *Outgoing) bool {
	if !s.cfg.Bounded {
		return true
	}
	n := len(s.queue) - s.exemptN
	bytes := s.queuedBytes + s.inflightBytes - s.exemptBytes
	return n+1 <= s.cfg.WindowMessages && bytes+o.size <= s.cfg.WindowBytes
}

func (s *Session) addLocked(o *Outgoing) {
	s.queue = append(s.queue, o)
	s.queuedBytes += o.size
	if o.Exempt {
		s.exemptN++
		s.exemptBytes += o.size
	}
}

// Reset 丢弃全部未确认的条目，改为只发送 o（窗口溢出转全量，NODE-14）。
func (s *Session) Reset(o *Outgoing) error {
	s.mu.Lock()
	if s.closeCode != 0 || s.err != nil {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	s.queue = nil
	s.nsent, s.inflightBytes, s.queuedBytes, s.exemptN, s.exemptBytes = 0, 0, 0, 0, 0
	s.epoch++
	s.addLocked(o)
	s.mu.Unlock()
	s.signal()
	return nil
}

// Unacked 返回尚未被确认的条目（按发送顺序），用于在新会话中重发。
func (s *Session) Unacked() []*Outgoing {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Outgoing(nil), s.queue...)
}

// closeGrace 是请求关闭后等待写协程完成关闭握手的上限；超过后直接中断连接。
const closeGrace = 5 * time.Second

// Close 请求以 code 关闭会话；实际的关闭由写协程执行，调用方不阻塞。
// 写协程因对端不读而阻塞时，closeGrace 之后中断连接。
func (s *Session) Close(code int, reason string) {
	s.mu.Lock()
	first := s.closeCode == 0
	if first {
		s.closeCode, s.closeReason = code, reason
		if s.err == nil {
			s.err = &LocalCloseError{Code: code, Reason: reason}
		}
	}
	s.mu.Unlock()
	s.signal()
	if first {
		s.graceTimer.Store(time.AfterFunc(closeGrace, s.cancel))
	}
}

// Abort 立即断开底层连接（模拟网络中断），并结束读写协程。
func (s *Session) Abort() {
	s.setErr(errAborted)
	_ = s.cfg.Conn.Abort()
	s.cancel()
}

// Ping 发送 WebSocket Ping 并等待 Pong（NODE-07）。
func (s *Session) Ping(ctx context.Context) error { return s.cfg.Conn.Ping(ctx) }

// LocalCloseError 表示本端主动关闭。
type LocalCloseError struct {
	Code   int
	Reason string
}

func (e *LocalCloseError) Error() string {
	return fmt.Sprintf("nodewire: closed locally (%d %s)", e.Code, e.Reason)
}

var errAborted = errors.New("nodewire: connection aborted")

// writeError 是写失败；对端发来关闭码时，写失败只是其后果，结束原因以关闭码为准（setPeerErr）。
type writeError struct{ err error }

func (e *writeError) Error() string { return "nodewire: write: " + e.err.Error() }
func (e *writeError) Unwrap() error { return e.err }

// setPeerErr 记录对端以关闭码结束会话：覆盖尚未记录的原因或写失败，不覆盖本端的关闭与协议错误。
func (s *Session) setPeerErr(err error) {
	s.mu.Lock()
	var we *writeError
	if s.err == nil || errors.As(s.err, &we) {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *Session) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *Session) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Session) protocolError(format string, args ...any) {
	err := fmt.Errorf("%w: "+format, append([]any{ErrProtocol}, args...)...)
	s.setErr(err)
	s.Close(CloseProtocolError, "protocol violation")
}

func (s *Session) readLoop() {
	var n uint64
	for {
		b, err := s.cfg.Conn.Read(s.ctx)
		if err != nil {
			switch re, ok := RejectFromError(err); {
			case ok:
				s.setPeerErr(re)
			case CloseCodeOf(err) >= 0:
				s.setPeerErr(fmt.Errorf("nodewire: read: %w", err))
			default:
				s.setErr(fmt.Errorf("nodewire: read: %w", err))
			}
			s.cancel()
			return
		}
		f, err := UnmarshalFrame(b)
		if err != nil {
			s.protocolError("%v", err)
			return
		}
		if r := f.GetHelloReject(); r != nil {
			// 已建立的会话被控制面以拒绝原因关闭（NODE-19 revoked、NODE-21 superseded）。
			s.setErr(&RejectError{Reason: r.GetReason(), RetryAfterMS: r.GetRetryAfterMs(), Detail: r.GetDetail()})
			s.cancel()
			return
		}
		sealed := f.GetSealed()
		if sealed == nil {
			s.protocolError("unexpected handshake frame after handshake")
			return
		}
		s.mu.Lock()
		expect := s.recvSeq + 1
		s.mu.Unlock()
		if f.GetSeq() != expect {
			// 重复、不递增或跳号（NODE-12）。
			s.protocolError("seq %d, expected %d", f.GetSeq(), expect)
			return
		}
		pt, err := s.opener.Open(f.GetSeq(), sealed)
		if err != nil {
			s.protocolError("seq %d: %v", f.GetSeq(), err)
			return
		}
		env := new(nodev1.Envelope)
		if err := proto.Unmarshal(pt, env); err != nil {
			s.protocolError("envelope: %v", err)
			return
		}
		s.mu.Lock()
		sent := s.sendSeq
		s.mu.Unlock()
		if env.GetAck() > sent {
			// 确认了本端从未发送的 seq（NODE-12）。
			s.protocolError("ack %d exceeds last sent seq %d", env.GetAck(), sent)
			return
		}
		n++
		if s.cfg.Faults != nil {
			a := s.cfg.Faults.Decide(Inbound, n, env)
			if a.Delay > 0 && !s.sleep(a.Delay) {
				return
			}
			if a.Disconnect {
				s.Abort()
				return
			}
			if a.Drop {
				// 视为本帧丢失：recvSeq 不前进，下一帧将被判为跳号。
				continue
			}
		}
		s.handle(f.GetSeq(), env)
	}
}

func (s *Session) handle(seq uint64, env *nodev1.Envelope) {
	ackOnly := env.GetAckOnly() != nil
	s.mu.Lock()
	s.recvSeq = seq
	s.stats.FramesReceived++
	var acked []*Outgoing
	if a := env.GetAck(); a > 0 {
		i := 0
		for i < s.nsent && s.queue[i].seq <= a {
			i++
		}
		if i > 0 {
			acked = append(acked, s.queue[:i]...)
			for _, o := range acked {
				s.inflightBytes -= o.size
				if o.Exempt {
					s.exemptN--
					s.exemptBytes -= o.size
				}
			}
			s.queue = append([]*Outgoing(nil), s.queue[i:]...)
			s.nsent -= i
		}
	}
	if !ackOnly {
		s.needAck = true
	}
	first := ackOnly || s.cfg.Dedup == nil || s.cfg.Dedup.FirstSeen(env.GetIdemKey())
	if !first {
		s.stats.Duplicates++
	}
	s.mu.Unlock()
	if len(acked) > 0 || !ackOnly {
		s.signal()
	}
	if s.cfg.OnReceive != nil {
		s.cfg.OnReceive(env)
	}
	if len(acked) > 0 && s.cfg.OnAcked != nil {
		s.cfg.OnAcked(acked)
	}
	if !ackOnly && first && s.cfg.Handler != nil {
		s.cfg.Handler(env)
	}
}

// next 在锁内选出下一条要发送的信封：先发队列中窗口允许的条目，否则在需要时发纯确认。
func (s *Session) next() (env *nodev1.Envelope, o *Outgoing, seq, epoch uint64, closeCode int, closeReason string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	epoch = s.epoch
	if s.closeCode != 0 {
		return nil, nil, 0, epoch, s.closeCode, s.closeReason, false
	}
	if s.nsent < len(s.queue) && (s.nsent == 0 || (s.nsent < s.cfg.WindowMessages && s.inflightBytes+s.queue[s.nsent].size <= s.cfg.WindowBytes)) {
		o = s.queue[s.nsent]
		s.nsent++
		s.inflightBytes += o.size
		s.queuedBytes -= o.size
	} else if !s.needAck {
		return nil, nil, 0, epoch, 0, "", false
	}
	s.sendSeq++
	s.needAck = false
	s.stats.FramesSent++
	env = &nodev1.Envelope{Ack: s.recvSeq, TsMs: s.cfg.Clock.Now().UnixMilli()}
	if o != nil {
		o.seq = s.sendSeq
		env.IdemKey, env.Body = o.Env.GetIdemKey(), o.Env.Body
	} else {
		env.IdemKey = NewUUIDv7(s.cfg.Clock, s.cfg.Rand)
		env.Body = &nodev1.Envelope_AckOnly{AckOnly: &nodev1.Ack{}}
	}
	return env, o, s.sendSeq, epoch, 0, "", true
}

func (s *Session) writeLoop() {
	for {
		env, o, seq, epoch, code, reason, ok := s.next()
		if code != 0 {
			_ = s.cfg.Conn.Close(code, reason)
			s.cancel()
			return
		}
		if !ok {
			select {
			case <-s.ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}
		var a Action
		if s.cfg.Faults != nil {
			a = s.cfg.Faults.Decide(Outbound, seq, env)
		}
		if a.Delay > 0 && !s.sleep(a.Delay) {
			return
		}
		if a.Disconnect {
			s.Abort()
			return
		}
		if a.Resend && o != nil {
			// 同一信封以新的 seq 再发一次（idem_key 不变），接收方按 NODE-13 去重。
			// 期间发生过 Reset（积压已丢弃）或窗口已满时不重发。
			dup := o.clone()
			dup.Key = ""
			s.mu.Lock()
			if s.epoch == epoch && s.closeCode == 0 && s.fitsLocked(dup) {
				s.addLocked(dup)
			}
			s.mu.Unlock()
		}
		if a.Drop {
			continue
		}
		b, err := s.seal(seq, env)
		if err != nil {
			s.setErr(err)
			s.Abort()
			return
		}
		times := 1
		if a.Duplicate {
			times = 2
		}
		for range times {
			if err := s.cfg.Conn.Write(s.ctx, b); err != nil {
				s.setErr(&writeError{err: err})
				_ = s.cfg.Conn.Abort()
				s.cancel()
				return
			}
		}
	}
}

func (s *Session) seal(seq uint64, env *nodev1.Envelope) ([]byte, error) {
	pt, err := proto.Marshal(env)
	if err != nil {
		return nil, err
	}
	sealed, err := s.sealer.Seal(seq, pt)
	if err != nil {
		return nil, err
	}
	return MarshalFrame(&nodev1.Frame{Kind: &nodev1.Frame_Sealed{Sealed: sealed}, Seq: seq})
}

func (s *Session) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.ctx.Done():
		return false
	}
}
