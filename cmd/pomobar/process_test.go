package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/clobrano/pomobar/internal/dbustest"
	"github.com/clobrano/pomobar/internal/fakewl"
)

// TestMain lets the test binary act as the pomobar binary, so tests can
// check real exit codes and real signal handling.
func TestMain(m *testing.M) {
	if os.Getenv("POMOBAR_RUN_MAIN") == "1" {
		os.Args = append([]string{"pomobar"}, strings.Split(os.Getenv("POMOBAR_ARGS"), "\x1f")...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
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

			cmd, stderr := pomobarCmd(t, []string{"XDG_RUNTIME_DIR=" + s.Dir(), "WAYLAND_DISPLAY=" + s.Name()}, "-label", "x", "25m")
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
	cmd, stderr := pomobarCmd(t, []string{"XDG_RUNTIME_DIR=" + s.Dir(), "WAYLAND_DISPLAY=" + s.Name()}, "25m")
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
