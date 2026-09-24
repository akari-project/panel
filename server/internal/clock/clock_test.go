// SPDX-License-Identifier: AGPL-3.0-or-later

package clock

import (
	"sync"
	"testing"
	"time"
)

func TestFake(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := NewFake(start)
	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("Now = %v, want %v", got, start)
	}
	if got := c.Advance(90 * time.Second); !got.Equal(start.Add(90 * time.Second)) {
		t.Fatalf("Advance = %v", got)
	}
	later := start.Add(24 * time.Hour)
	c.Set(later)
	if got := c.Now(); !got.Equal(later) {
		t.Fatalf("after Set, Now = %v, want %v", got, later)
	}
}

func TestFakeConcurrent(t *testing.T) {
	c := NewFake(time.Unix(0, 0))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.Advance(time.Millisecond)
				_ = c.Now()
			}
		}()
	}
	wg.Wait()
	if got := c.Now(); !got.Equal(time.Unix(0, 0).Add(800 * time.Millisecond)) {
		t.Fatalf("Now = %v", got)
	}
}

func TestRealImplementsClock(t *testing.T) {
	var c Clock = Real{}
	if c.Now().IsZero() {
		t.Fatal("Real.Now returned zero time")
	}
}
