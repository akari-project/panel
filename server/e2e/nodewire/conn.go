// SPDX-License-Identifier: AGPL-3.0-or-later

package nodewire

import (
	"context"
	"errors"
	"fmt"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"
)

// MaxFrameSize 是单帧的读取上限：快照可能较大，取窗口字节上限（NODE-14 的 8 MiB）加余量。
const MaxFrameSize = 8<<20 + 64<<10

// Conn 是一条传输连接上的帧收发（NODE-05 的 WebSocket；长轮询 M5-02 可以实现同一接口）。
// Read 与 Write 可以在不同协程中并发调用；Close 与 Abort 可以在任意协程中调用。
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, frame []byte) error
	// Close 发送关闭码后关闭连接，可能阻塞到对方确认或超时。
	Close(code int, reason string) error
	// Abort 立即关闭底层连接，不发送关闭帧（模拟网络中断）。
	Abort() error
	Ping(ctx context.Context) error
}

type wsConn struct{ c *websocket.Conn }

// WrapWebSocket 把 WebSocket 连接适配为 Conn，只接受二进制消息。
func WrapWebSocket(c *websocket.Conn) Conn {
	c.SetReadLimit(MaxFrameSize)
	return wsConn{c: c}
}

func (w wsConn) Read(ctx context.Context) ([]byte, error) {
	typ, b, err := w.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		_ = w.c.Close(websocket.StatusUnsupportedData, "binary frames only")
		return nil, fmt.Errorf("%w: text message", ErrProtocol)
	}
	return b, nil
}

func (w wsConn) Write(ctx context.Context, frame []byte) error {
	return w.c.Write(ctx, websocket.MessageBinary, frame)
}

func (w wsConn) Close(code int, reason string) error {
	return w.c.Close(websocket.StatusCode(code), reason)
}

func (w wsConn) Abort() error { return w.c.CloseNow() }

func (w wsConn) Ping(ctx context.Context) error { return w.c.Ping(ctx) }

// CloseCodeOf 返回连接错误中的 WebSocket 关闭码；没有关闭码时返回 -1。
func CloseCodeOf(err error) int { return int(websocket.CloseStatus(err)) }

// ErrProtocol 表示对端违反协议（序号、帧类型、认证失败等），本端关闭连接。
var ErrProtocol = errors.New("nodewire: protocol violation")

// 本实现使用的非拒绝类关闭码。
const (
	CloseNormal        = int(websocket.StatusNormalClosure)
	CloseGoingAway     = int(websocket.StatusGoingAway)
	CloseProtocolError = int(websocket.StatusProtocolError)
	ClosePolicy        = int(websocket.StatusPolicyViolation)
)

// RejectError 表示握手被拒绝，或已建立的会话被控制面以拒绝原因关闭（NODE-19、NODE-21、NODE-22）。
type RejectError struct {
	Reason       nodev1.HelloRejectReason
	RetryAfterMS uint32
	Detail       string
	// FromCloseCode 为 true 表示只收到了关闭码，没有收到 hello_reject 帧。
	FromCloseCode bool
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("nodewire: rejected: %s (retry_after_ms=%d) %s", e.Reason, e.RetryAfterMS, e.Detail)
}

// RejectFromError 从连接错误中提取拒绝原因：hello_reject 帧或 4001–4007 关闭码。
func RejectFromError(err error) (*RejectError, bool) {
	var re *RejectError
	if errors.As(err, &re) {
		return re, true
	}
	if r, ok := ReasonFromCloseCode(CloseCodeOf(err)); ok {
		return &RejectError{Reason: r, FromCloseCode: true}, true
	}
	return nil, false
}

// MarshalFrame 序列化一个帧。
func MarshalFrame(f *nodev1.Frame) ([]byte, error) { return proto.Marshal(f) }

// UnmarshalFrame 解析一个帧。
func UnmarshalFrame(b []byte) (*nodev1.Frame, error) {
	f := new(nodev1.Frame)
	if err := proto.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("%w: frame: %v", ErrProtocol, err)
	}
	return f, nil
}

// SendReject 发送 hello_reject 后以对应关闭码关闭连接（NODE-22）。
func SendReject(ctx context.Context, c Conn, r *nodev1.HelloReject) {
	if b, err := MarshalFrame(&nodev1.Frame{Kind: &nodev1.Frame_HelloReject{HelloReject: r}}); err == nil {
		_ = c.Write(ctx, b)
	}
	_ = c.Close(CloseCode(r.GetReason()), r.GetReason().String())
}
