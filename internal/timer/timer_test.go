package timer

import (
	"errors"
	"image/color"
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

func TestNewRejectsNonPositive(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := New(d, t0); !errors.Is(err, ErrNonPositive) {
			t.Errorf("New(%v) error = %v, want ErrNonPositive", d, err)
		}
	}
}

func TestFraction(t *testing.T) {
	tm, err := New(100*time.Second, t0)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		at   time.Duration
		frac float64
		rem  time.Duration
		done bool
	}{
		{0, 1, 100 * time.Second, false},
		{-5 * time.Second, 1, 100 * time.Second, false}, // clock went backwards
		{25 * time.Second, 0.75, 75 * time.Second, false},
		{50 * time.Second, 0.5, 50 * time.Second, false},
		{99*time.Second + 500*time.Millisecond, 0.005, 500 * time.Millisecond, false},
		{100 * time.Second, 0, 0, true},
		{time.Hour, 0, 0, true},
	}
	for _, c := range cases {
		now := t0.Add(c.at)
		if got := tm.Fraction(now); math.Abs(got-c.frac) > 1e-12 {
			t.Errorf("Fraction(+%v) = %v, want %v", c.at, got, c.frac)
		}
		if got := tm.Remaining(now); got != c.rem {
			t.Errorf("Remaining(+%v) = %v, want %v", c.at, got, c.rem)
		}
		if got := tm.Done(now); got != c.done {
			t.Errorf("Done(+%v) = %v, want %v", c.at, got, c.done)
		}
	}
	if tm.Deadline() != t0.Add(100*time.Second) || tm.Duration() != 100*time.Second {
		t.Errorf("Deadline/Duration = %v/%v", tm.Deadline(), tm.Duration())
	}
}

// The deadline must follow the wall clock, not the monotonic clock, which
// stops during suspend on Linux.
func TestUsesWallClock(t *testing.T) {
	start := time.Now() // carries a monotonic reading
	tm, err := New(25*time.Minute, start)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a resume after a 30 minute suspend: the wall clock moved
	// 30 minutes while the monotonic reading moved only one second.
	// time.Time has no API to build such a value, so check the
	// observable consequences instead: no monotonic reading is kept, and
	// a wall-clock-only "now" past the deadline is done.
	if s := tm.Deadline().String(); containsMonotonic(s) {
		t.Fatalf("deadline keeps a monotonic reading: %s", s)
	}
	resumed := start.Round(0).Add(30 * time.Minute)
	if !tm.Done(resumed) {
		t.Fatal("timer not done 30 wall-clock minutes after start")
	}
	if got := tm.Remaining(start.Add(10 * time.Minute)); got != 15*time.Minute {
		t.Fatalf("Remaining = %v, want 15m", got)
	}
}

func containsMonotonic(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i:i+3] == "m=+" || s[i:i+3] == "m=-" {
			return true
		}
	}
	return false
}

func TestColorAtThresholds(t *testing.T) {
	cases := []struct {
		f    float64
		want color.RGBA
	}{
		{1, Green},
		{0.75, Green},
		{0.50, Green},
		{0.45, Amber},
		{0.30, Amber},
		{0.20, Amber},
		{0.15, Red},
		{0.05, Red},
		{0, Red},
		{-1, Red},
		{2, Green},
		{math.NaN(), Red},
	}
	for _, c := range cases {
		if got := Color(c.f); got != c.want {
			t.Errorf("Color(%v) = %v, want %v", c.f, got, c.want)
		}
	}
}

func TestColorBlendsAroundThresholds(t *testing.T) {
	mid := Color(AmberThreshold - BlendWidth/2)
	if mid == Green || mid == Amber || mid.A != 0xFF {
		t.Errorf("Color midway green→amber = %v, want a blend", mid)
	}
	mid = Color(RedThreshold - BlendWidth/2)
	if mid == Amber || mid == Red || mid.A != 0xFF {
		t.Errorf("Color midway amber→red = %v, want a blend", mid)
	}
	// Just inside each blend the color is next to the pure color.
	if VisiblyDifferent(Color(AmberThreshold-1e-6), Green) {
		t.Error("discontinuity at the amber threshold")
	}
	if VisiblyDifferent(Color(AmberThreshold-BlendWidth+1e-6), Amber) {
		t.Error("discontinuity at the end of the green→amber blend")
	}
	if VisiblyDifferent(Color(RedThreshold-1e-6), Amber) {
		t.Error("discontinuity at the red threshold")
	}
	if VisiblyDifferent(Color(RedThreshold-BlendWidth+1e-6), Red) {
		t.Error("discontinuity at the end of the amber→red blend")
	}
}

// Within each blend every channel moves monotonically from one color to
// the other, and the color is continuous over the whole range.
func TestColorMonotonic(t *testing.T) {
	segments := []struct{ hi, lo float64 }{
		{AmberThreshold, AmberThreshold - BlendWidth},
		{RedThreshold, RedThreshold - BlendWidth},
	}
	for _, s := range segments {
		from, to := Color(s.hi), Color(s.lo)
		prev := from
		const steps = 1000
		for i := 1; i <= steps; i++ {
			f := s.hi - (s.hi-s.lo)*float64(i)/steps
			c := Color(f)
			for _, ch := range [][4]uint8{
				{from.R, to.R, prev.R, c.R},
				{from.G, to.G, prev.G, c.G},
				{from.B, to.B, prev.B, c.B},
			} {
				a, b, p, x := ch[0], ch[1], ch[2], ch[3]
				if (b >= a && x < p) || (b < a && x > p) {
					t.Fatalf("channel not monotonic at %v: %v -> %v", f, prev, c)
				}
			}
			prev = c
		}
	}
	prev := Color(1)
	for i := 1; i <= 10000; i++ {
		c := Color(1 - float64(i)/10000)
		if VisiblyDifferent(prev, c) && maxDelta(prev, c) > 8 {
			t.Fatalf("jump at %v: %v -> %v", 1-float64(i)/10000, prev, c)
		}
		prev = c
	}
}

func maxDelta(a, b color.RGBA) int {
	d := func(x, y uint8) int { return int(math.Abs(float64(x) - float64(y))) }
	return max(d(a.R, b.R), d(a.G, b.G), d(a.B, b.B))
}

func TestVisiblyDifferent(t *testing.T) {
	a := color.RGBA{10, 10, 10, 255}
	if VisiblyDifferent(a, a) {
		t.Error("identical colors reported different")
	}
	if VisiblyDifferent(a, color.RGBA{11, 10, 10, 255}) {
		t.Error("1-step change reported visible")
	}
	if !VisiblyDifferent(a, color.RGBA{10, 10, 12, 255}) {
		t.Error("2-step change not reported visible")
	}
}

func TestFilledLength(t *testing.T) {
	cases := []struct {
		f      float64
		length int
		want   int
	}{
		{1, 1920, 1920},
		{0, 1920, 0},
		{0.5, 1920, 960},
		{0.5, 3, 2},  // 1.5 rounds half away from zero
		{0.49, 3, 1}, // 1.47
		{1.0 / 1920, 1920, 1},
		{0.4 / 1920, 1920, 0},
		{0.6 / 1920, 1920, 1},
		{1.5, 100, 100},
		{-0.1, 100, 0},
		{0.5, 0, 0},
		{math.NaN(), 100, 0},
	}
	for _, c := range cases {
		if got := FilledLength(c.f, c.length); got != c.want {
			t.Errorf("FilledLength(%v, %d) = %d, want %d", c.f, c.length, got, c.want)
		}
	}
}

func TestPauseResume(t *testing.T) {
	tm, _ := New(10*time.Minute, t0)
	if tm.Resume(t0) {
		t.Fatal("Resume of a running timer succeeded")
	}
	if !tm.Pause(t0.Add(4 * time.Minute)) {
		t.Fatal("Pause failed")
	}
	if tm.Pause(t0.Add(5*time.Minute)) || !tm.Paused() {
		t.Fatal("second Pause succeeded or timer not paused")
	}
	// Time passes while paused (including a suspend): nothing changes.
	later := t0.Add(2 * time.Hour)
	if got := tm.Remaining(later); got != 6*time.Minute {
		t.Fatalf("Remaining while paused = %v, want 6m", got)
	}
	if tm.Done(later) || tm.Fraction(later) != 0.6 {
		t.Fatalf("paused timer: done=%v fraction=%v", tm.Done(later), tm.Fraction(later))
	}
	if !tm.Resume(later) || tm.Paused() {
		t.Fatal("Resume failed")
	}
	if got := tm.Remaining(later.Add(time.Minute)); got != 5*time.Minute {
		t.Fatalf("Remaining after resume = %v, want 5m", got)
	}
	if !tm.Done(later.Add(6 * time.Minute)) {
		t.Fatal("not done 6m after resume")
	}
	if tm.Pause(later.Add(7 * time.Minute)) {
		t.Fatal("Pause of a finished timer succeeded")
	}
}

func TestDim(t *testing.T) {
	if got := Dim(Green); got != (color.RGBA{0x26, 0x57, 0x28, 0x7F}) {
		t.Fatalf("Dim(Green) = %v", got)
	}
}
