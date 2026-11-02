package main

import (
	"bytes"
	"errors"
	"image/color"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/clobrano/pomobar/internal/bar"
	"github.com/clobrano/pomobar/internal/dbustest"
	"github.com/clobrano/pomobar/internal/fakewl"
	"github.com/clobrano/pomobar/internal/ipc"
	"github.com/clobrano/pomobar/internal/notify"
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
		{args: "-foreground 5m", want: config{duration: 5 * time.Minute, edge: bar.Bottom, thickness: 4, foreground: true}},
		{args: "status", want: config{edge: bar.Bottom, thickness: 4, cmd: "status"}},
		{args: "stop", want: config{edge: bar.Bottom, thickness: 4, cmd: "stop"}},
		{args: "pause", want: config{edge: bar.Bottom, thickness: 4, cmd: "pause"}},
		{args: "resume", want: config{edge: bar.Bottom, thickness: 4, cmd: "resume"}},
		{args: "status now", wantErr: "unexpected arguments: now"},
		{args: "-edge top stop", wantErr: "stop takes no flags (got -edge)"},
		{args: "25m stop", wantErr: `invalid break "stop"`},
		{args: "", wantErr: "missing duration"},
		{args: "25m 5m", want: config{duration: 25 * time.Minute, rest: 5 * time.Minute, cycles: 1, edge: bar.Bottom, thickness: 4}},
		{args: "25m 5m 4", want: config{duration: 25 * time.Minute, rest: 5 * time.Minute, cycles: 4, edge: bar.Bottom, thickness: 4}},
		{args: "-label x 50m 10m 2 -edge top", want: config{duration: 50 * time.Minute, rest: 10 * time.Minute, cycles: 2, edge: bar.Top, thickness: 4, label: "x"}},
		{args: "next", want: config{edge: bar.Bottom, thickness: 4, cmd: "next"}},
		{args: "25m 5m 4 1", wantErr: "unexpected arguments: 1"},
		{args: "25m 0s 4", wantErr: `invalid break "0s": must be positive`},
		{args: "25m five 4", wantErr: `invalid break "five"`},
		{args: "25m 5m 0", wantErr: `invalid number of cycles "0"`},
		{args: "25m 5m 2.5", wantErr: `invalid number of cycles "2.5"`},
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
func (f *fakeBar) Sync() error           { return nil }
func (f *fakeBar) Done() <-chan struct{} { return f.done }
func (f *fakeBar) Err() error            { return f.err }
func (f *fakeBar) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// fakeNotifier records notifications.
type fakeNotifier struct {
	mu        sync.Mutex
	bodies    []string // of "Time's up" notifications
	all       []string // "summary|body|action" of every notification
	err       error
	askErr    error
	noApprove bool // questions are never answered
	withdrawn int  // questions closed before being answered
}

func (n *fakeNotifier) Ask(summary, body, action string) (*notify.Question, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.askErr != nil {
		return nil, n.askErr
	}
	n.all = append(n.all, summary+"|"+body+"|"+action)
	answered := make(chan struct{})
	if !n.noApprove {
		close(answered)
	}
	return notify.NewQuestion(answered, func() {
		select {
		case <-answered:
		default:
			n.mu.Lock()
			n.withdrawn++
			n.mu.Unlock()
		}
	}), nil
}

func (n *fakeNotifier) log() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.all...)
}

func (n *fakeNotifier) Notify(summary, body string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if summary == "Time's up" {
		n.bodies = append(n.bodies, body)
	}
	n.all = append(n.all, summary+"|"+body)
	return n.err
}

func testDeps(b *fakeBar, now func() time.Time) (deps, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return deps{
		openBar:  func(o bar.Options) (drawer, error) { b.opts = o; return b, nil },
		notifier: &fakeNotifier{},
		now:      now,
		tick:     time.Millisecond,
		stdout:   &out,
		stderr:   &errb,
	}, &out, &errb
}

func TestUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{{}, {"25m", "x"}, {"25m", "5m", "4", "x"}, {"-edge", "up", "1m"}, {"-thickness", "0", "1m"}, {"0s"}} {
		d, _, errb := testDeps(newFakeBar(), time.Now)
		if code := run(args, d); code != exitUsage {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
		if !strings.Contains(errb.String(), "Usage:\n  pomobar [flags] <duration>") {
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
	if code := run([]string{"-version"}, d); code != exitOK || out.String() != "pomobar dev\n" {
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
	if code := run([]string{"-foreground", "-edge", "right", "-reserve", "-thickness", "3", "10m"}, d); code != exitOK {
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
	if code := run([]string{"-foreground", "25m"}, d); code != exitOK {
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
	if code := run([]string{"-foreground", "1h"}, d); code != exitError {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "closed the bar") || !b.closed {
		t.Fatalf("stderr %q closed %v", errb.String(), b.closed)
	}
	if n := d.notifier.(*fakeNotifier); len(n.bodies) != 0 {
		t.Fatal("notified after the compositor closed the bar")
	}
}

func TestOpenErrorExits1(t *testing.T) {
	for _, err := range []error{bar.ErrNoDisplay, bar.ErrNoLayerShell} {
		var errb bytes.Buffer
		n := &fakeNotifier{}
		d := deps{
			openBar:  func(bar.Options) (drawer, error) { return nil, err },
			notifier: n,
			now:      time.Now, tick: time.Second, stdout: &bytes.Buffer{}, stderr: &errb,
		}
		if code := run([]string{"-foreground", "25m"}, d); code != exitError {
			t.Fatalf("exit %d, want 1", code)
		}
		if got := errb.String(); got != "pomobar: "+err.Error()+"\n" {
			t.Fatalf("stderr %q", got)
		}
		if len(n.bodies) != 0 {
			t.Fatal("notified after an error")
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

	srv := dbustest.StartNotificationServer(t, dbustest.StartBus(t))

	d := defaultDeps()
	var errb bytes.Buffer
	d.stderr = &errb
	d.tick = 100 * time.Millisecond
	start := time.Now()
	if code := run([]string{"-foreground", "-edge", "top", "-label", "Break time", "1500ms"}, d); code != exitOK {
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
	if first.Width != 400 || first.Pixel(0, 0) != 0xFF4CAF50 || first.Pixel(390, 0) != 0xFF4CAF50 {
		t.Fatalf("first frame %dx%d px=%#x", first.Width, first.Height, first.Pixel(390, 0))
	}
	calls := srv.Wait(1, 5*time.Second)
	if len(calls) != 1 || calls[0].AppName != "pomobar" || calls[0].Summary != "Time's up" || calls[0].Body != "Break time" {
		t.Fatalf("notifications: %+v", calls)
	}
}

// TestLongRun runs a real timer of $POMOBAR_LONG_RUN (e.g. 25m) against the
// fake compositor and checks it ends within one second of the wall-clock
// deadline. Skipped unless the variable is set:
//
//	POMOBAR_LONG_RUN=25m go test -run TestLongRun -timeout 30m -v ./cmd/pomobar
func TestLongRun(t *testing.T) {
	v := os.Getenv("POMOBAR_LONG_RUN")
	if v == "" {
		t.Skip("set POMOBAR_LONG_RUN to a duration to run")
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
	if code := run([]string{"-foreground", "-quiet", v}, d); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	end := time.Now().Round(0)
	late := end.Sub(wallStart) - dur
	t.Logf("duration %v, ended %v after the wall-clock deadline, %d frames", dur, late, len(s.Snapshot().Frames))
	if late < 0 || late > time.Second {
		t.Fatalf("ended %v after the deadline, want within 1s", late)
	}
}

func fastClock() func() time.Time {
	c := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), step: time.Minute}
	return c.now
}

func TestNotificationAtEnd(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"-label", "Write the report", "5m"}, []string{"Write the report"}},
		{[]string{"5m"}, []string{"Your 5m0s timer has finished."}},
		{[]string{"-quiet", "-label", "x", "5m"}, nil},
	}
	for _, c := range cases {
		b := newFakeBar()
		d, _, errb := testDeps(b, fastClock())
		if code := run(append([]string{"-foreground"}, c.args...), d); code != exitOK {
			t.Fatalf("%q: exit %d: %s", c.args, code, errb)
		}
		n := d.notifier.(*fakeNotifier)
		if strings.Join(n.bodies, "|") != strings.Join(c.want, "|") {
			t.Errorf("%q: notifications %q, want %q", c.args, n.bodies, c.want)
		}
		if !b.closed {
			t.Errorf("%q: bar not removed", c.args)
		}
	}
}

func TestNotificationFailureKeepsExit0(t *testing.T) {
	d, _, errb := testDeps(newFakeBar(), fastClock())
	d.notifier = &fakeNotifier{err: errors.New("no server\non the bus")}
	if code := run([]string{"-foreground", "5m"}, d); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if got := errb.String(); got != "pomobar: warning: notification failed: no server on the bus\n" {
		t.Fatalf("stderr %q", got)
	}
}

func TestSignalsCancel(t *testing.T) {
	for sig, want := range map[os.Signal]int{syscall.SIGINT: 130, syscall.SIGTERM: 143} {
		b := newFakeBar()
		d, _, _ := testDeps(b, time.Now)
		ch := make(chan os.Signal, 1)
		ch <- sig
		d.signals = ch
		if code := run([]string{"-foreground", "25m"}, d); code != want {
			t.Errorf("%v: exit %d, want %d", sig, code, want)
		}
		if !b.closed {
			t.Errorf("%v: bar not removed", sig)
		}
		if n := d.notifier.(*fakeNotifier); len(n.bodies) != 0 {
			t.Errorf("%v: notified on cancel", sig)
		}
	}
}

func TestPomodoroSequence(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), step: time.Minute}
	b := newFakeBar()
	d, _, errb := testDeps(b, clk.now)
	if code := run([]string{"-foreground", "-label", "Write", "3m", "2m", "2"}, d); code != exitOK {
		t.Fatalf("exit %d: %s", code, errb)
	}
	want := []string{
		"Time for a break|Write: Cycle 1/2 done: take a 2m0s break.",
		`Break is over|Write: Ready for cycle 2/2? Start it here or run "pomobar next".|Start cycle 2/2`,
		"Time for a break|Write: Cycle 2/2 done: take a 2m0s break.",
		"Pomodoro complete|Write: All 2 cycles done.",
	}
	if got := d.notifier.(*fakeNotifier).log(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notifications:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Work is green, breaks blue, waiting a dimmed full green bar.
	var sawBlue, sawWaiting bool
	for i, c := range b.colors {
		if c == timer.Blue {
			sawBlue = true
		}
		if c == timer.Dim(timer.Green) && b.draws[i] == 1 {
			sawWaiting = true
		}
	}
	if !sawBlue || !sawWaiting {
		t.Fatalf("colors: blue=%v waiting=%v (%v)", sawBlue, sawWaiting, b.colors)
	}
}

func TestPomodoroQuietWaitsForNext(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), step: time.Minute}
	b := newFakeBar()
	d, _, _ := testDeps(b, clk.now)
	n := d.notifier.(*fakeNotifier)
	got := make(chan int, 1)
	go func() { got <- run([]string{"-foreground", "-quiet", "2m", "1m", "2"}, d) }()
	path, _ := ipc.SocketPath()
	var st ipc.State
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := ipc.Call(path, ipc.Request{Cmd: ipc.CmdStatus}); err == nil && resp.State.Phase == ipc.PhaseWaiting {
			st = resp.State
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st.Phase != ipc.PhaseWaiting || st.Cycle != 2 || st.Cycles != 2 {
		t.Fatalf("state %+v, want waiting for cycle 2/2", st)
	}
	for _, cmd := range []string{ipc.CmdPause, ipc.CmdResume} {
		if resp, _ := ipc.Call(path, ipc.Request{Cmd: cmd}); resp.OK {
			t.Fatalf("%s accepted while waiting", cmd)
		}
	}
	if resp, err := ipc.Call(path, ipc.Request{Cmd: ipc.CmdNext}); err != nil || !resp.OK {
		t.Fatalf("next: %+v %v", resp, err)
	}
	if code := <-got; code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if l := n.log(); len(l) != 0 {
		t.Fatalf("-quiet sent notifications: %q", l)
	}
}

func TestPomodoroWithoutActionButtons(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), step: time.Minute}
	d, _, _ := testDeps(newFakeBar(), clk.now)
	n := d.notifier.(*fakeNotifier)
	n.askErr = notify.ErrNoActions
	got := make(chan int, 1)
	go func() { got <- run([]string{"-foreground", "2m", "1m", "2"}, d) }()
	path, _ := ipc.SocketPath()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := ipc.Call(path, ipc.Request{Cmd: ipc.CmdNext}); err == nil && resp.OK {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if code := <-got; code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if l := n.log(); len(l) < 2 || l[1] != `Break is over|Run "pomobar next" to start cycle 2/2.` {
		t.Fatalf("notifications %q", l)
	}
}

func TestNextWithdrawsQuestion(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), step: time.Minute}
	d, _, _ := testDeps(newFakeBar(), clk.now)
	n := d.notifier.(*fakeNotifier)
	n.noApprove = true
	got := make(chan int, 1)
	go func() { got <- run([]string{"-foreground", "2m", "1m", "2"}, d) }()
	path, _ := ipc.SocketPath()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := ipc.Call(path, ipc.Request{Cmd: ipc.CmdNext}); err == nil && resp.OK {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if code := <-got; code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if n.withdrawn != 1 {
		t.Fatalf("withdrawn %d, want 1", n.withdrawn)
	}
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		st   ipc.State
		want string
	}{
		{ipc.State{Phase: ipc.PhaseWork, Duration: 25 * time.Minute, Remaining: 723 * time.Second}, "12:03 left of 25:00"},
		{ipc.State{Phase: ipc.PhaseWork, Duration: 2 * time.Hour, Remaining: 3601 * time.Second, Paused: true, Label: "x"}, "1:00:01 left of 2:00:00 (paused) — x"},
		{ipc.State{Phase: ipc.PhaseBreak, Duration: 5 * time.Minute, Remaining: 190 * time.Second, Cycle: 2, Cycles: 4}, "break 2/4: 3:10 left of 5:00"},
		{ipc.State{Phase: ipc.PhaseWaiting, Cycle: 3, Cycles: 4, Label: "x"}, `waiting to start cycle 3/4 (run "pomobar next") — x`},
		{ipc.State{Phase: ipc.PhaseWork, Duration: time.Minute, Remaining: 1500 * time.Millisecond}, "0:02 left of 1:00"},
	}
	for _, c := range cases {
		if got := describe(c.st); got != c.want {
			t.Errorf("describe(%+v) = %q, want %q", c.st, got, c.want)
		}
	}
}
