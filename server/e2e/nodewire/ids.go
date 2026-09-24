// SPDX-License-Identifier: AGPL-3.0-or-later

package nodewire

import (
	"encoding/binary"
	"io"
	"sync"
	"time"

	"github.com/akari-project/panel/server/internal/clock"
)

// NewUUIDv7 按 RFC 9562 §5.7 生成 UUIDv7 文本：时间取自注入的时钟（CONV-04），随机部分取自 r。
// 用于 Envelope.idem_key（NODE-13）与测试用的租约 ID。
func NewUUIDv7(c clock.Clock, r io.Reader) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(c.Now().UnixMilli())<<16)
	if _, err := io.ReadFull(r, b[6:]); err != nil {
		panic("nodewire: random source failed: " + err.Error())
	}
	b[6] = 0x70 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	return FormatUUID(b[:])
}

// DedupRetention 是接收方保留 idem_key 去重记录的最短时间（NODE-13）。
const DedupRetention = 24 * time.Hour

// Dedup 记录已处理的 idem_key，跨会话保留，并发安全。
type Dedup struct {
	clock     clock.Clock
	retention time.Duration

	mu      sync.Mutex
	seen    map[string]time.Time
	inserts int
}

// NewDedup 返回保留 retention 的去重表；retention 为 0 时取 DedupRetention。
func NewDedup(c clock.Clock, retention time.Duration) *Dedup {
	if retention == 0 {
		retention = DedupRetention
	}
	return &Dedup{clock: c, retention: retention, seen: make(map[string]time.Time)}
}

// FirstSeen 记录 key，第一次出现时返回 true。空 key 不去重。
func (d *Dedup) FirstSeen(key string) bool {
	if key == "" {
		return true
	}
	now := d.clock.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.seen[key]; ok && now.Sub(t) < d.retention {
		return false
	}
	d.seen[key] = now
	d.inserts++
	if d.inserts%1024 == 0 {
		for k, t := range d.seen {
			if now.Sub(t) >= d.retention {
				delete(d.seen, k)
			}
		}
	}
	return true
}

// Len 返回当前保留的记录数。
func (d *Dedup) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// LockedReader 把 r 包装为并发安全的随机源（crypto/rand.Reader 本身已经并发安全）。
func LockedReader(r io.Reader) io.Reader {
	if r == nil {
		return nil
	}
	if _, ok := r.(*lockedReader); ok {
		return r
	}
	return &lockedReader{r: r}
}

type lockedReader struct {
	mu sync.Mutex
	r  io.Reader
}

func (l *lockedReader) Read(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.Read(p)
}
