package lanmdns

import "time"

type actorTimer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

type actorClock interface {
	Now() time.Time
	NewTimer(time.Duration) actorTimer
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) NewTimer(duration time.Duration) actorTimer {
	return realTimer{Timer: time.NewTimer(duration)}
}

type realTimer struct{ *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.Timer.C }
