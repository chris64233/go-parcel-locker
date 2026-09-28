package goparcellocker

import "time"

// Clock 抽象当前时间，便于测试驱动超时推进而无需真实睡眠。
type Clock interface {
	Now() time.Time
}

// systemClock 使用本机墙上时间。
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// fakeClock 是测试用时钟：时间只能被显式推进，保证并发测试可重复。
type fakeClock struct {
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time { return c.now }

// advance 将时钟向前移动 d。
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }
