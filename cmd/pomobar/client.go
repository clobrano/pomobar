package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/clobrano/pomobar/internal/ipc"
)

// readyFDEnv names the file descriptor on which a detached timer reports
// whether it started ("OK" or "ERR <message>").
const readyFDEnv = "POMOBAR_READY_FD"

// startTimeout bounds how long `pomobar <duration>` waits for the detached
// timer to come up.
const startTimeout = 10 * time.Second

// runClient sends a command to the running timer and prints the result.
func runClient(cmd string, d deps) int {
	path, err := ipc.SocketPath()
	if err != nil {
		fmt.Fprintln(d.stderr, "pomobar:", err)
		return exitError
	}
	resp, err := ipc.Call(path, ipc.Request{Cmd: cmd})
	if err != nil {
		fmt.Fprintln(d.stderr, "pomobar:", err)
		return exitError
	}
	if !resp.OK {
		fmt.Fprintln(d.stderr, "pomobar:", resp.Error)
		return exitError
	}
	st := resp.State
	switch cmd {
	case ipc.CmdStatus:
		fmt.Fprintln(d.stdout, describe(st))
	case ipc.CmdStop:
		fmt.Fprintf(d.stdout, "stopped with %s left\n", clock(st.Remaining))
	case ipc.CmdPause:
		fmt.Fprintf(d.stdout, "paused with %s left\n", clock(st.Remaining))
	case ipc.CmdResume:
		fmt.Fprintf(d.stdout, "resumed with %s left\n", clock(st.Remaining))
	case ipc.CmdNext:
		fmt.Fprintf(d.stdout, "starting cycle %d/%d\n", st.Cycle, st.Cycles)
	}
	return exitOK
}

// describe formats a status line, e.g. `12:03 left of 25:00 (paused) — Write`
// or, for a pomodoro, `break 2/4: 3:10 left of 5:00 — Write`.
func describe(st ipc.State) string {
	var s string
	if st.Phase == ipc.PhaseWaiting {
		s = fmt.Sprintf("waiting to start cycle %d/%d (run \"pomobar next\")", st.Cycle, st.Cycles)
	} else {
		s = fmt.Sprintf("%s left of %s", clock(st.Remaining), clock(st.Duration))
		if st.Paused {
			s += " (paused)"
		}
		if st.Cycles > 0 {
			s = fmt.Sprintf("%s %d/%d: %s", st.Phase, st.Cycle, st.Cycles, s)
		}
	}
	if st.Label != "" {
		s += " — " + st.Label
	}
	return s
}

// clock formats a duration as m:ss or h:mm:ss, rounding up to the second
// so a running timer never shows 0:00.
func clock(d time.Duration) string {
	secs := int64((d + time.Second - 1) / time.Second)
	h, m, s := secs/3600, secs/60%60, secs%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// alreadyRunning describes the running timer for a refused start.
func alreadyRunning(path string) error {
	msg := "a timer is already running"
	if resp, err := ipc.Call(path, ipc.Request{Cmd: ipc.CmdStatus}); err == nil && resp.OK {
		msg += " (" + describe(resp.State) + ")"
	}
	return errors.New(msg + `; use "pomobar stop" first`)
}

// startDetached starts the timer in a background process and returns once
// its bar is up.
func startDetached(args []string, d deps) int {
	path, err := ipc.SocketPath()
	if err != nil {
		fmt.Fprintln(d.stderr, "pomobar:", err)
		return exitError
	}
	if _, err := ipc.Call(path, ipc.Request{Cmd: ipc.CmdStatus}); err == nil {
		fmt.Fprintln(d.stderr, "pomobar:", alreadyRunning(path))
		return exitError
	}
	if err := d.spawn(args); err != nil {
		fmt.Fprintln(d.stderr, "pomobar:", err)
		return exitError
	}
	return exitOK
}

// spawnDetached re-executes pomobar with -foreground in a new session,
// with no terminal, and waits for it to report that it started.
func spawnDetached(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find the pomobar executable: %w", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		w.Close()
		return err
	}
	defer devnull.Close()

	cmd := exec.Command(exe, append([]string{"-foreground"}, args...)...)
	cmd.Env = append(os.Environ(), readyFDEnv+"=3")
	cmd.ExtraFiles = []*os.File{w} // fd 3 in the child
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	w.Close()
	if err != nil {
		return fmt.Errorf("cannot start the timer process: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()

	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		done <- result{strings.TrimSpace(line), err}
	}()
	select {
	case res := <-done:
		switch {
		case res.line == "OK":
			return nil
		case strings.HasPrefix(res.line, "ERR "):
			return errors.New(strings.TrimPrefix(res.line, "ERR "))
		default:
			return errors.New("the timer process exited before starting")
		}
	case <-time.After(startTimeout):
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return errors.New("the timer process did not start in time")
	}
}

// readyPipeFromEnv returns the readiness pipe passed by spawnDetached, if
// any, and hides the variable from processes we start later.
func readyPipeFromEnv() io.WriteCloser {
	v := os.Getenv(readyFDEnv)
	if v == "" {
		return nil
	}
	os.Unsetenv(readyFDEnv)
	fd, err := strconv.Atoi(v)
	if err != nil || fd < 3 {
		return nil
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "ready")
}

// reportReady tells the parent of a detached timer whether it started.
// It does nothing when no parent is waiting.
func reportReady(d deps, err error) {
	if d.ready == nil {
		return
	}
	if err != nil {
		fmt.Fprintf(d.ready, "ERR %s\n", oneLine(err))
	} else {
		fmt.Fprintln(d.ready, "OK")
	}
	d.ready.Close()
}
