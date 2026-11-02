package ipc

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCallRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), SocketName)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go Serve(ln, func(r Request) Response {
		if r.Cmd != CmdStatus {
			return Response{Error: "unknown command " + r.Cmd}
		}
		return Response{OK: true, State: State{Phase: PhaseBreak, Duration: 25 * time.Minute, Remaining: time.Minute, Paused: true, Label: "x", Cycle: 2, Cycles: 4}}
	})
	resp, err := Call(path, Request{Cmd: CmdStatus})
	if err != nil {
		t.Fatal(err)
	}
	want := State{Phase: PhaseBreak, Duration: 25 * time.Minute, Remaining: time.Minute, Paused: true, Label: "x", Cycle: 2, Cycles: 4}
	if !resp.OK || resp.State != want {
		t.Fatalf("resp %+v", resp)
	}
	resp, err = Call(path, Request{Cmd: "bogus"})
	if err != nil || resp.OK || resp.Error != "unknown command bogus" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
}

func TestListenRefusesLiveTimer(t *testing.T) {
	path := filepath.Join(t.TempDir(), SocketName)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := Listen(path); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Listen = %v, want ErrRunning", err)
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), SocketName)
	// Leave a socket file behind with nobody listening.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	ln.Close()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket not removed on close: %v", err)
	}
}

func TestCallNotRunning(t *testing.T) {
	if _, err := Call(filepath.Join(t.TempDir(), SocketName), Request{Cmd: CmdStatus}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}

func TestSocketPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := SocketPath(); !errors.Is(err, ErrNoRuntimeDir) {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if p, _ := SocketPath(); p != "/run/user/1000/pomobar.sock" {
		t.Fatalf("path %q", p)
	}
}

// Serve must not return before an in-flight response has been written,
// even when the listener is closed while the handler runs.
func TestServeWaitsForResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), SocketName)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	inHandler := make(chan struct{})
	served := make(chan struct{})
	go func() {
		defer close(served)
		Serve(ln, func(Request) Response {
			close(inHandler)
			time.Sleep(100 * time.Millisecond)
			return Response{OK: true}
		})
	}()
	got := make(chan error, 1)
	go func() {
		resp, err := Call(path, Request{Cmd: CmdStop})
		if err == nil && !resp.OK {
			err = errors.New("not ok")
		}
		got <- err
	}()
	<-inHandler
	ln.Close()
	<-served
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("client got no response")
	}
}
