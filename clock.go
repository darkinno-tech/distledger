package distledger

import (
	"sync"
	"time"
)

// Clock 抽象当前时间。
//
// 库内部**禁止**直接调用 time.Now()：所有时间都必须来自注入的 Clock。
// 这样冻结期、结算时点、绑定期限在测试中就可以被瞬间推进，不需要 sleep，
// 也不会出现「测试跑了 7 天」这种不可接受的用例（见 ADR-007）。
type Clock interface {
	// Now 返回当前时间。实现必须返回 UTC 时间。
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// SystemClock 返回使用系统时间的 Clock，这是默认实现。
func SystemClock() Clock { return systemClock{} }

// ManualClock 是一个可手动推进的 Clock，供测试与仿真使用。
//
// 它是并发安全的。零值不可用，请使用 NewManualClock 构造。
type ManualClock struct {
	mu  sync.RWMutex
	now time.Time
}

// NewManualClock 返回一个停在 t（按 UTC 归一）的 ManualClock。
func NewManualClock(t time.Time) *ManualClock {
	return &ManualClock{now: t.UTC()}
}

// Now 返回当前被设定的时间。
func (c *ManualClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

// Advance 把时钟向前推进 d。d 为负时回拨。
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set 把时钟设置为 t。
func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC()
}
