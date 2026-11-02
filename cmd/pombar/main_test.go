package main

import (
	"bytes"
	"image/color"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clobrano/pomobar/internal/bar"
	"github.com/clobrano/pomobar/internal/fakewl"
	"github.com/clobrano/pomobar/internal/timer"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		args    string
		want    config
		wantErr string
	}{
		{args: "25m", want: config{duration: 25 * time.Minute, edge: bar.Bottom, thickness: 4}},
		{args: "-edge top -thickness 6 -reserve -label x -quiet 1h30m",
			want: config{duration: 90 * time.Minute, edge: bar.Top, thickness: 6, reserve: true, label: "x", quiet: true}},
		{args: "90s -edge left", want: config{duration: 90 * time.Second, edge: bar.Left, thickness: 4}},
		{args: "-version", want: config{edge: bar.Bottom, thickness: 4, version: true}},
		{args: "", wantErr: "missing duration"},
		{args: "25m 5m", wantErr: "unexpected arguments: 5m"},
		{args: "0s", wantErr: "must be positive"},
		{args: "-5m", wantErr: "flag provided but not defined"},
		{args: "-- -5m", wantErr: "must be positive"},
		{args: "25", wantErr: "invalid duration"},
		{args: "-edge middle 25m", wantErr: "unknown edge"},
		{args: "-thickness 0 25m", wantErr: "invalid thickness"},
		{args: "-bogus 25m", wantErr: "flag provided but not defined"},
	}
	for _, c := range cases {
		cfg, err := parseArgs(strings.Fields(c.args))
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("parseArgs(%q) error = %v, want %q", c.args, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseArgs(%q): %v", c.args, err)
			continue
		}
		if cfg != c.want {
			t.Errorf("parseArgs(%q) = %+v, want %+v", c.args, cfg, c.want)
		}
	}
}

// fakeBar records draws.
type fakeBar struct {
	mu      sync.Mutex
	opts    bar.Options
	draws   []float64
	colors  []color.RGBA
	closed  bool
	done    chan struct{}
	err     error
	drawErr error
}

func newFakeBar() *fakeBar { return &fakeBar{done: make(chan struct{})} }

func (f *fakeBar) Draw(fr float64, c color.RGBA) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.draws = append(f.draws, fr)
	f.colors = append(f.colors, c)
	return f.drawErr
}
func (f *fakeBar) Done() <-chan struct{} { return f.done }
func (f *fakeBar) Err() error            { return f.err }
func (f *fakeBar) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func testDeps(b *fakeBar, now func() time.Time) (deps, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return deps{
		openBar: func(o bar.Options) (drawer, error) { b.opts = o; return b, nil },
		now:     now,
		tick:    time.Millisecond,
		stdout:  &out,
		stderr:  &errb,
	}, &out, &errb
}

func TestUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{{}, {"25m", "x"}, {"-edge", "up", "1m"}, {"-thickness", "0", "1m"}, {"0s"}} {
		d, _, errb := testDeps(newFakeBar(), time.Now)
		if code := run(args, d); code != exitUsage {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
		if !strings.Contains(errb.String(), "Usage: pombar") {
			t.Errorf("run(%q) did not print usage", args)
		}
	}
}

func TestHelpAndVersion(t *testing.T) {
	d, out, _ := testDeps(newFakeBar(), time.Now)
	if code := run([]string{"-h"}, d); code != exitOK || !strings.Contains(out.String(), "Examples:") {
		t.Fatalf("-h: code %d output %q", code, out.String())
	}
	d, out, _ = testDeps(newFakeBar(), time.Now)
	if code := run([]string{"-version"}, d); code != exitOK || out.String() != "pombar dev\n" {
		t.Fatalf("-version: code %d output %q", code, out.String())
	}
}

// fakeClock advances by step on every call.
type fakeClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.t
	c.t = c.t.Add(c.step)
	return t
}

func TestCountdownDrawsShrinkingBarAndColors(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), step: time.Minute}
	b := newFakeBar()
	d, _, _ := testDeps(b, clk.now)
	if code := run([]string{"-edge", "right", "-reserve", "-thickness", "3", "10m"}, d); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if b.opts != (bar.Options{Edge: bar.Right, Thickness: 3, Reserve: true}) {
		t.Errorf("bar options %+v", b.opts)
	}
	if !b.closed {
		t.Error("bar not closed")
	}
	// The clock advances a minute per reading, and the timer's start
	// consumed the first one.
	if len(b.draws) != 9 {
		t.Fatalf("draws = %v, want 9 (0.9 .. 0.1)", b.draws)
	}
	for i, f := range b.draws {
		if want := 0.9 - float64(i)/10; f < want-1e-9 || f > want+1e-9 {
			t.Errorf("draw %d = %v, want %v", i, f, want)
		}
		if b.colors[i] != timer.Color(f) {
			t.Errorf("draw %d color %v, want %v", i, b.colors[i], timer.Color(f))
		}
	}
}

// A suspend moves the wall clock without the process running; the timer
// must end on the first wake-up after resume.
func TestCountdownEndsAfterSuspend(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	now := func() time.Time {
		calls++
		if calls <= 3 {
			return start.Add(time.Duration(calls-1) * time.Second)
		}
		return start.Add(40 * time.Minute) // resumed after a long suspend
	}
	b := newFakeBar()
	d, _, _ := testDeps(b, now)
	d.tick = 10 * time.Millisecond
	begin := time.Now()
	if code := run([]string{"25m"}, d); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if el := time.Since(begin); el > time.Second {
		t.Fatalf("took %v after resume", el)
	}
}

func TestBarClosedByCompositorExits1(t *testing.T) {
	b := newFakeBar()
	b.err = bar.ErrClosed
	close(b.done)
	d, _, errb := testDeps(b, time.Now)
	if code := run([]string{"1h"}, d); code != exitError {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "closed the bar") || !b.closed {
		t.Fatalf("stderr %q closed %v", errb.String(), b.closed)
	}
}

func TestOpenErrorExits1(t *testing.T) {
	for _, err := range []error{bar.ErrNoDisplay, bar.ErrNoLayerShell} {
		var errb bytes.Buffer
		d := deps{
			openBar: func(bar.Options) (drawer, error) { return nil, err },
			now:     time.Now, tick: time.Second, stdout: &bytes.Buffer{}, stderr: &errb,
		}
		if code := run([]string{"25m"}, d); code != exitError {
			t.Fatalf("exit %d, want 1", code)
		}
		if got := errb.String(); got != "pombar: "+err.Error()+"\n" {
			t.Fatalf("stderr %q", got)
		}
	}
}

// End to end against the fake compositor with the real bar and clock: the
// timer must end on time and remove the bar.
func TestEndToEndFakeCompositor(t *testing.T) {
	s, err := fakewl.Start(fakewl.Options{OutputWidth: 400})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t.Setenv("XDG_RUNTIME_DIR", s.Dir())
	t.Setenv("WAYLAND_DISPLAY", s.Name())

	d := defaultDeps()
	var errb bytes.Buffer
	d.stderr = &errb
	d.tick = 100 * time.Millisecond
	start := time.Now()
	if code := run([]string{"-edge", "top", "1500ms"}, d); code != exitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	late := time.Since(start) - 1500*time.Millisecond
	if late < 0 || late > time.Second {
		t.Fatalf("ended %v after the deadline, want within 1s", late)
	}
	st, err := s.WaitFor(5*time.Second, func(st fakewl.State) bool { return st.Disconnected })
	if err != nil {
		t.Fatal(err)
	}
	if !st.Layer.Destroyed || len(st.Frames) < 2 || st.ProtocolError != "" {
		t.Fatalf("destroyed=%v frames=%d err=%q", st.Layer.Destroyed, len(st.Frames), st.ProtocolError)
	}
	first := st.Frames[0]
	if first.Width != 400 || first.Pixel(399, 0) != 0xFF4CAF50 {
		t.Fatalf("first frame %dx%d px=%#x", first.Width, first.Height, first.Pixel(399, 0))
	}
}

// TestLongRun runs a real timer of $POMBAR_LONG_RUN (e.g. 25m) against the
// fake compositor and checks it ends within one second of the wall-clock
// deadline. Skipped unless the variable is set:
//
//	POMBAR_LONG_RUN=25m go test -run TestLongRun -timeout 30m -v ./cmd/pombar
func TestLongRun(t *testing.T) {
	v := os.Getenv("POMBAR_LONG_RUN")
	if v == "" {
		t.Skip("set POMBAR_LONG_RUN to a duration to run")
	}
	dur, err := time.ParseDuration(v)
	if err != nil {
		t.Fatal(err)
	}
	s, err := fakewl.Start(fakewl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t.Setenv("XDG_RUNTIME_DIR", s.Dir())
	t.Setenv("WAYLAND_DISPLAY", s.Name())
	d := defaultDeps()
	start := time.Now()
	wallStart := start.Round(0)
	if code := run([]string{"-quiet", v}, d); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	end := time.Now().Round(0)
	late := end.Sub(wallStart) - dur
	t.Logf("duration %v, ended %v after the wall-clock deadline, %d frames", dur, late, len(s.Snapshot().Frames))
	if late < 0 || late > time.Second {
		t.Fatalf("ended %v after the deadline, want within 1s", late)
	}
}
