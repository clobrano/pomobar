package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/clobrano/pomobar/internal/dbustest"
	"github.com/clobrano/pomobar/internal/fakewl"
	"github.com/clobrano/pomobar/internal/ipc"
)

// TestMain lets the test binary act as the pomobar binary, so tests can
// check real exit codes and real signal handling.
func TestMain(m *testing.M) {
	if os.Getenv("POMOBAR_RUN_MAIN") == "1" {
		// A detached timer is re-executed with its own argv, starting
		// with -foreground; otherwise the test passed args in the env.
		if len(os.Args) < 2 || os.Args[1] != "-foreground" {
			os.Args = append([]string{"pomobar"}, strings.Split(os.Getenv("POMOBAR_ARGS"), "\x1f")...)
		}
		main()
		os.Exit(0)
	}
	// Keep in-process tests' control sockets out of the real runtime dir.
	dir, err := os.MkdirTemp("", "pomobar-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_RUNTIME_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// pomobarCmd builds a command running main with args and extra env.
func pomobarCmd(t *testing.T, env []string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "POMOBAR_RUN_MAIN=1", "POMOBAR_ARGS="+strings.Join(args, "\x1f"))
	cmd.Env = append(cmd.Env, env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return cmd, &stderr
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatal(err)
	}
	return ee.ExitCode()
}

func TestProcessSignals(t *testing.T) {
	for _, c := range []struct {
		sig  syscall.Signal
		code int
	}{{syscall.SIGINT, 130}, {syscall.SIGTERM, 143}} {
		t.Run(c.sig.String(), func(t *testing.T) {
			s, err := fakewl.Start(fakewl.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			srv := dbustest.StartNotificationServer(t, dbustest.StartBus(t))

			cmd, stderr := pomobarCmd(t, []string{"XDG_RUNTIME_DIR=" + s.Dir(), "WAYLAND_DISPLAY=" + s.Name()}, "-foreground", "-label", "x", "25m")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.WaitFor(10*time.Second, func(st fakewl.State) bool { return len(st.Frames) > 0 }); err != nil {
				_ = cmd.Process.Kill()
				t.Fatal(err)
			}
			sent := time.Now()
			_ = cmd.Process.Signal(c.sig)
			code := exitCode(t, cmd.Wait())
			if code != c.code {
				t.Fatalf("exit %d, want %d; stderr %q", code, c.code, stderr)
			}
			if el := time.Since(sent); el > time.Second {
				t.Errorf("took %v to exit", el)
			}
			st := s.Snapshot()
			if !st.Layer.Destroyed || st.SurfaceAlive || st.SyncsAfterGone == 0 {
				t.Errorf("bar not torn down: %+v", st.Layer)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr %q", stderr)
			}
			time.Sleep(200 * time.Millisecond)
			if n := srv.Calls(); len(n) != 0 {
				t.Errorf("cancelled timer sent %d notifications", len(n))
			}
		})
	}
}

func TestProcessErrors(t *testing.T) {
	noLS, err := fakewl.Start(fakewl.Options{NoLayerShell: true})
	if err != nil {
		t.Fatal(err)
	}
	defer noLS.Close()

	cases := []struct {
		name   string
		env    []string
		args   []string
		code   int
		stderr string
		stdout string
	}{
		{"no WAYLAND_DISPLAY", []string{"WAYLAND_DISPLAY="}, []string{"25m"}, 1,
			"pomobar: WAYLAND_DISPLAY is not set: not running in a Wayland session\n", ""},
		{"no layer-shell", []string{"XDG_RUNTIME_DIR=" + noLS.Dir(), "WAYLAND_DISPLAY=" + noLS.Name()}, []string{"25m"}, 1,
			"pomobar: the compositor does not support wlr-layer-shell (zwlr_layer_shell_v1)\n", ""},
		{"no compositor", []string{"XDG_RUNTIME_DIR=" + t.TempDir(), "WAYLAND_DISPLAY=wayland-9"}, []string{"25m"}, 1,
			"pomobar: cannot connect to the Wayland compositor: ", ""},
		{"usage", nil, []string{"-edge", "middle", "25m"}, 2, "pomobar: unknown edge", ""},
		{"version", []string{"WAYLAND_DISPLAY="}, []string{"-version"}, 0, "", "pomobar dev\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, stderr := pomobarCmd(t, c.env, c.args...)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			code := exitCode(t, cmd.Run())
			if code != c.code {
				t.Fatalf("exit %d, want %d; stderr %q", code, c.code, stderr)
			}
			if !strings.HasPrefix(stderr.String(), c.stderr) || (c.stderr == "" && stderr.Len() != 0) {
				t.Errorf("stderr %q, want prefix %q", stderr, c.stderr)
			}
			if c.code == 1 && strings.Count(stderr.String(), "\n") != 1 {
				t.Errorf("error is not one line: %q", stderr)
			}
			if stdout.String() != c.stdout {
				t.Errorf("stdout %q, want %q", stdout.String(), c.stdout)
			}
		})
	}
}

func TestProcessCompositorClosesBar(t *testing.T) {
	s, err := fakewl.Start(fakewl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	srv := dbustest.StartNotificationServer(t, dbustest.StartBus(t))
	cmd, stderr := pomobarCmd(t, []string{"XDG_RUNTIME_DIR=" + s.Dir(), "WAYLAND_DISPLAY=" + s.Name()}, "-foreground", "25m")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WaitFor(10*time.Second, func(st fakewl.State) bool { return len(st.Frames) > 0 }); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	if err := s.SendClosed(); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(t, cmd.Wait()); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := stderr.String(); got != "pomobar: the compositor closed the bar surface\n" {
		t.Fatalf("stderr %q", got)
	}
	if len(srv.Calls()) != 0 {
		t.Fatal("notified after the compositor closed the bar")
	}
}

// runPomobar runs a pomobar command to completion.
func runPomobar(t *testing.T, env []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd, errb := pomobarCmd(t, env, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	code = exitCode(t, cmd.Run())
	return code, out.String(), errb.String()
}

// sessionEnv starts a fake compositor and a session bus with a
// notification server, in a private runtime dir.
func sessionEnv(t *testing.T) (*fakewl.Server, *dbustest.Server, []string) {
	t.Helper()
	s, err := fakewl.Start(fakewl.Options{OutputWidth: 400})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := dbustest.StartNotificationServer(t, dbustest.StartBus(t))
	env := []string{"XDG_RUNTIME_DIR=" + s.Dir(), "WAYLAND_DISPLAY=" + s.Name()}
	// Never leave a detached timer behind if the test fails.
	t.Cleanup(func() {
		_, _ = ipc.Call(filepath.Join(s.Dir(), ipc.SocketName), ipc.Request{Cmd: ipc.CmdStop})
	})
	return s, srv, env
}

func TestDetachedLifecycle(t *testing.T) {
	s, srv, env := sessionEnv(t)

	begin := time.Now()
	code, out, errOut := runPomobar(t, env, "-label", "Write", "25m")
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("start: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if el := time.Since(begin); el > 5*time.Second {
		t.Fatalf("start blocked for %v", el)
	}
	// The bar is already mapped when the starting command returns.
	if st := s.Snapshot(); len(st.Frames) == 0 || !st.Layer.Acked {
		t.Fatalf("bar not shown when start returned: frames=%d", len(st.Frames))
	}

	code, out, _ = runPomobar(t, env, "status")
	if code != 0 || !strings.HasSuffix(out, " left of 25:00 — Write\n") || !strings.HasPrefix(out, "2") {
		t.Fatalf("status: exit %d %q", code, out)
	}

	code, _, errOut = runPomobar(t, env, "5m")
	if code != 1 || !strings.HasPrefix(errOut, "pomobar: a timer is already running (") ||
		!strings.HasSuffix(errOut, `; use "pomobar stop" first`+"\n") {
		t.Fatalf("second start: exit %d stderr %q", code, errOut)
	}

	code, out, _ = runPomobar(t, env, "pause")
	if code != 0 || !strings.HasPrefix(out, "paused with ") {
		t.Fatalf("pause: exit %d %q", code, out)
	}
	if _, err := s.WaitFor(5*time.Second, func(st fakewl.State) bool {
		f, ok := st.LastFrame()
		return ok && f.Pixel(0, 0)>>24 == 0x7F
	}); err != nil {
		t.Fatalf("bar not dimmed while paused: %v", err)
	}
	code, out, _ = runPomobar(t, env, "status")
	if code != 0 || !strings.Contains(out, "(paused)") {
		t.Fatalf("status while paused: %q", out)
	}
	code, _, errOut = runPomobar(t, env, "pause")
	if code != 1 || errOut != "pomobar: the timer is already paused\n" {
		t.Fatalf("second pause: exit %d %q", code, errOut)
	}
	code, out, _ = runPomobar(t, env, "resume")
	if code != 0 || !strings.HasPrefix(out, "resumed with ") {
		t.Fatalf("resume: exit %d %q", code, out)
	}

	code, out, _ = runPomobar(t, env, "stop")
	if code != 0 || !strings.HasPrefix(out, "stopped with ") {
		t.Fatalf("stop: exit %d %q", code, out)
	}
	st, err := s.WaitFor(5*time.Second, func(st fakewl.State) bool { return st.Disconnected })
	if err != nil || !st.Layer.Destroyed {
		t.Fatalf("bar not removed after stop: %v", err)
	}
	code, _, errOut = runPomobar(t, env, "status")
	if code != 1 || errOut != "pomobar: no timer is running\n" {
		t.Fatalf("status after stop: exit %d %q", code, errOut)
	}
	time.Sleep(200 * time.Millisecond)
	if n := srv.Calls(); len(n) != 0 {
		t.Fatalf("stopped timer sent %d notifications", len(n))
	}
}

func TestDetachedCompletes(t *testing.T) {
	s, srv, env := sessionEnv(t)
	if code, _, errOut := runPomobar(t, env, "-label", "Break", "1s"); code != 0 {
		t.Fatalf("start: exit %d %q", code, errOut)
	}
	calls := srv.Wait(1, 10*time.Second)
	if len(calls) != 1 || calls[0].Body != "Break" {
		t.Fatalf("notifications %+v", calls)
	}
	if _, err := s.WaitFor(5*time.Second, func(st fakewl.State) bool { return st.Disconnected }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), ipc.SocketName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control socket left behind: %v", err)
	}
}

func TestForegroundStoppedByClient(t *testing.T) {
	s, _, env := sessionEnv(t)
	cmd, stderr := pomobarCmd(t, env, "-foreground", "25m")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WaitFor(10*time.Second, func(st fakewl.State) bool { return len(st.Frames) > 0 }); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	if code, _, errOut := runPomobar(t, env, "stop"); code != 0 {
		t.Fatalf("stop: exit %d %q", code, errOut)
	}
	if code := exitCode(t, cmd.Wait()); code != 3 {
		t.Fatalf("foreground exit %d, want 3; stderr %q", code, stderr)
	}
}

func TestClientWithoutTimer(t *testing.T) {
	env := []string{"XDG_RUNTIME_DIR=" + t.TempDir()}
	for _, c := range []string{"status", "stop", "pause", "resume"} {
		if code, _, errOut := runPomobar(t, env, c); code != 1 || errOut != "pomobar: no timer is running\n" {
			t.Errorf("%s: exit %d %q", c, code, errOut)
		}
	}
}

// A detached pomodoro of two short cycles: the second cycle starts only
// when the notification's action button is pressed.
func TestDetachedPomodoroActionButton(t *testing.T) {
	s, srv, env := sessionEnv(t)
	if code, _, errOut := runPomobar(t, env, "-label", "Write", "500ms", "500ms", "2"); code != 0 {
		t.Fatalf("start: exit %d %q", code, errOut)
	}
	calls := srv.Wait(2, 10*time.Second)
	if len(calls) != 2 || calls[0].Summary != "Time for a break" || calls[1].Summary != "Break is over" ||
		len(calls[1].Actions) != 2 || calls[1].Actions[1] != "Start cycle 2/2" || calls[1].ExpireTimeout != 0 {
		t.Fatalf("notifications %+v", calls)
	}

	// Nothing happens until the user approves.
	time.Sleep(700 * time.Millisecond)
	code, out, _ := runPomobar(t, env, "status")
	if code != 0 || out != `waiting to start cycle 2/2 (run "pomobar next") — Write`+"\n" {
		t.Fatalf("status while waiting: exit %d %q", code, out)
	}
	if n := len(srv.Calls()); n != 2 {
		t.Fatalf("%d notifications while waiting", n)
	}
	// A full bar in dimmed green (premultiplied 0x7F265728).
	if f, ok := s.Snapshot().LastFrame(); !ok || f.Pixel(0, 0) != 0x7F265728 || f.Pixel(399, 0) != 0x7F265728 {
		t.Fatalf("waiting bar is not a full dimmed bar")
	}

	if err := srv.Invoke(2, "start"); err != nil {
		t.Fatal(err)
	}
	calls = srv.Wait(4, 10*time.Second)
	if len(calls) != 4 || calls[2].Body != "Write: Cycle 2/2 done: take a 500ms break." ||
		calls[3].Summary != "Pomodoro complete" || calls[3].Body != "Write: All 2 cycles done." {
		t.Fatalf("notifications %+v", calls)
	}
	if _, err := s.WaitFor(5*time.Second, func(st fakewl.State) bool { return st.Disconnected }); err != nil {
		t.Fatal(err)
	}
	// The break bar was blue.
	var blue bool
	for _, f := range s.Snapshot().Frames {
		if f.Pixel(0, 0) == 0xFF42A5F5 {
			blue = true
		}
	}
	if !blue {
		t.Fatal("no blue break frame")
	}
}

// Without action buttons the user starts the next cycle with `pomobar next`.
func TestDetachedPomodoroNext(t *testing.T) {
	_, srv, env := sessionEnv(t)
	srv.SetActions(false)
	// Work phases long enough for the client checks below to run inside
	// cycle 2's, even under the race detector.
	if code, _, errOut := runPomobar(t, env, "2s", "300ms", "2"); code != 0 {
		t.Fatalf("start: exit %d %q", code, errOut)
	}
	calls := srv.Wait(2, 10*time.Second)
	if len(calls) != 2 || calls[1].Body != `Run "pomobar next" to start cycle 2/2.` || len(calls[1].Actions) != 0 {
		t.Fatalf("notifications %+v", calls)
	}
	if code, _, errOut := runPomobar(t, env, "pause"); code != 1 || !strings.Contains(errOut, `run "pomobar next"`) {
		t.Fatalf("pause while waiting: exit %d %q", code, errOut)
	}
	code, out, _ := runPomobar(t, env, "next")
	if code != 0 || out != "starting cycle 2/2\n" {
		t.Fatalf("next: exit %d %q", code, out)
	}
	code, out, _ = runPomobar(t, env, "status")
	if code != 0 || !strings.HasPrefix(out, "work 2/2: ") {
		t.Fatalf("status: exit %d %q", code, out)
	}
	if code, _, errOut := runPomobar(t, env, "next"); code != 1 || errOut != "pomobar: the timer is not waiting to start a cycle\n" {
		t.Fatalf("next while running: exit %d %q", code, errOut)
	}
	calls = srv.Wait(4, 10*time.Second)
	if len(calls) != 4 || calls[3].Summary != "Pomodoro complete" {
		t.Fatalf("notifications %+v", calls)
	}
}

// `pomobar next` while the question is shown withdraws it.
func TestNextClosesQuestion(t *testing.T) {
	_, srv, env := sessionEnv(t)
	if code, _, errOut := runPomobar(t, env, "300ms", "300ms", "2"); code != 0 {
		t.Fatalf("start: exit %d %q", code, errOut)
	}
	if calls := srv.Wait(2, 10*time.Second); len(calls) != 2 {
		t.Fatalf("notifications %+v", calls)
	}
	if code, _, errOut := runPomobar(t, env, "next"); code != 0 {
		t.Fatalf("next: exit %d %q", code, errOut)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.Closed()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c := srv.Closed(); len(c) != 1 || c[0] != 2 {
		t.Fatalf("withdrawn %v, want [2]", c)
	}
}
