// Package ipc is the control protocol between a running pomobar timer and
// the pomobar client commands (status, stop, pause, resume).
//
// The timer listens on a unix socket in $XDG_RUNTIME_DIR. Each connection
// carries one JSON request line and one JSON response line.
package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// SocketName is the socket file name inside $XDG_RUNTIME_DIR.
const SocketName = "pomobar.sock"

// Commands.
const (
	CmdStatus = "status"
	CmdStop   = "stop"
	CmdPause  = "pause"
	CmdResume = "resume"
)

// ioTimeout bounds every read and write on a control connection.
const ioTimeout = 3 * time.Second

var (
	// ErrNoRuntimeDir is returned when $XDG_RUNTIME_DIR is not set.
	ErrNoRuntimeDir = errors.New("XDG_RUNTIME_DIR is not set")
	// ErrNotRunning is returned by Call when no timer is running.
	ErrNotRunning = errors.New("no timer is running")
	// ErrRunning is returned by Listen when another timer already owns
	// the socket.
	ErrRunning = errors.New("a timer is already running")
)

// Request is sent by a client.
type Request struct {
	Cmd string `json:"cmd"`
}

// State describes the running timer.
type State struct {
	Duration  time.Duration `json:"duration"`
	Remaining time.Duration `json:"remaining"`
	Paused    bool          `json:"paused"`
	Label     string        `json:"label,omitempty"`
}

// Response is the timer's answer.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	State State  `json:"state"`
}

// SocketPath returns the control socket path.
func SocketPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", ErrNoRuntimeDir
	}
	return filepath.Join(dir, SocketName), nil
}

// Listen claims the control socket at path. If a live timer answers on it,
// it returns ErrRunning; a stale socket left by a crashed timer is
// replaced. Closing the listener removes the socket file.
func Listen(path string) (*net.UnixListener, error) {
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err == nil {
		return ln, nil
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return nil, err
	}
	if c, derr := net.DialTimeout("unix", path, ioTimeout); derr == nil {
		c.Close()
		return nil, ErrRunning
	}
	// Nobody listens: a stale socket. Remove it and try once more; if
	// another pomobar won the race, report it as running.
	if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return nil, rerr
	}
	ln, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if errors.Is(err, syscall.EADDRINUSE) {
		return nil, ErrRunning
	}
	return ln, err
}

// Call sends one request to the timer at path.
func Call(path string, req Request) (Response, error) {
	var resp Response
	c, err := net.DialTimeout("unix", path, ioTimeout)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return resp, ErrNotRunning
		}
		return resp, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(ioTimeout))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return resp, fmt.Errorf("send request: %w", err)
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return resp, fmt.Errorf("read response: %w", err)
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return resp, fmt.Errorf("bad response: %w", err)
	}
	return resp, nil
}

// Handler answers one request. It is called on the connection's goroutine.
type Handler func(Request) Response

// Serve accepts connections on ln until it is closed, answering each
// request with h. It returns once ln is closed and every response in
// flight has been written, so a process may exit right after it returns.
func Serve(ln *net.UnixListener, h Handler) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		wg.Go(func() { serveConn(c, h) })
	}
}

func serveConn(c *net.UnixConn, h Handler) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(ioTimeout))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var req Request
	var resp Response
	if err := json.Unmarshal(line, &req); err != nil {
		resp = Response{Error: "malformed request"}
	} else {
		resp = h(req)
	}
	_ = json.NewEncoder(c).Encode(resp)
}
