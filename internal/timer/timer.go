package timer

import (
	"errors"
	"time"
)

// ErrNonPositive is returned for a zero or negative duration.
var ErrNonPositive = errors.New("duration must be positive")

// Timer is a countdown with an absolute wall-clock deadline.
//
// Times are stripped of Go's monotonic clock reading (time.Time.Round(0)),
// so every comparison uses the wall clock. On Linux the monotonic clock
// (CLOCK_MONOTONIC) stops while the system is suspended; a deadline based on
// it would end late by however long the laptop slept. The wall clock keeps
// counting across suspend, so a 25 m timer started before a suspend ends 25
// wall-clock minutes after it started. The trade-off is that a manual clock
// change or an NTP step moves the deadline by the same amount.
//
// A paused timer keeps its remaining time frozen; resuming moves the
// deadline to now plus that remaining time.
type Timer struct {
	start    time.Time
	deadline time.Time
	duration time.Duration
	paused   bool
	frozen   time.Duration // remaining time while paused
}

// New starts a timer of duration d at now.
func New(d time.Duration, now time.Time) (*Timer, error) {
	if d <= 0 {
		return nil, ErrNonPositive
	}
	start := now.Round(0)
	return &Timer{start: start, deadline: start.Add(d), duration: d}, nil
}

// Duration is the total length of the timer.
func (t *Timer) Duration() time.Duration { return t.duration }

// Deadline is the wall-clock time at which the timer ends (if it is not
// paused).
func (t *Timer) Deadline() time.Time { return t.deadline }

// Remaining is the time left at now, never negative and never more than
// the duration (if the clock went backwards).
func (t *Timer) Remaining(now time.Time) time.Duration {
	if t.paused {
		return t.frozen
	}
	r := t.deadline.Sub(now.Round(0))
	return min(max(r, 0), t.duration)
}

// Paused reports whether the timer is paused.
func (t *Timer) Paused() bool { return t.paused }

// Pause freezes the remaining time. It returns false if the timer is
// already paused or done.
func (t *Timer) Pause(now time.Time) bool {
	if t.paused || t.Done(now) {
		return false
	}
	t.frozen = t.Remaining(now)
	t.paused = true
	return true
}

// Resume restarts a paused timer with the time it had left. It returns
// false if the timer is not paused.
func (t *Timer) Resume(now time.Time) bool {
	if !t.paused {
		return false
	}
	t.deadline = now.Round(0).Add(t.frozen)
	t.paused = false
	return true
}

// Fraction is the remaining fraction of the timer at now, in [0, 1]:
// 1 at the start, 0 at and after the deadline.
func (t *Timer) Fraction(now time.Time) float64 {
	return float64(t.Remaining(now)) / float64(t.duration)
}

// Done reports whether the deadline has passed at now.
func (t *Timer) Done(now time.Time) bool {
	return t.Remaining(now) <= 0
}
