// Package dbustest runs a private dbus-daemon session bus and a fake
// org.freedesktop.Notifications server for tests.
package dbustest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

const busConfig = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:path=%s</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`

// StartBus starts a private session bus, points $DBUS_SESSION_BUS_ADDRESS
// at it for the rest of the test and returns its address. The test is
// skipped if dbus-daemon is not installed.
func StartBus(t testing.TB) string {
	t.Helper()
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon not installed")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "bus.conf")
	if err := os.WriteFile(conf, fmt.Appendf(nil, busConfig, filepath.Join(dir, "bus")), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(daemon, "--config-file="+conf, "--nofork", "--nopidfile", "--print-address=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard // dbus-daemon warns about fd limits in containers
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	addrCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		addrCh <- strings.TrimSpace(line)
	}()
	var addr string
	select {
	case addr = <-addrCh:
	case <-time.After(10 * time.Second):
		t.Fatal("dbus-daemon did not print its address")
	}
	if addr == "" {
		t.Fatal("dbus-daemon printed no address")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", addr)
	return addr
}

// Notification is one received Notify call.
type Notification struct {
	AppName       string
	ReplacesID    uint32
	Icon          string
	Summary       string
	Body          string
	Actions       []string
	Hints         map[string]dbus.Variant
	ExpireTimeout int32
}

// Server is a fake notification server.
type Server struct {
	mu    sync.Mutex
	cond  *sync.Cond
	calls []Notification
}

// Notify implements org.freedesktop.Notifications.Notify.
func (s *Server) Notify(app string, replaces uint32, icon, summary, body string, actions []string, hints map[string]dbus.Variant, timeout int32) (uint32, *dbus.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Notification{app, replaces, icon, summary, body, actions, hints, timeout})
	s.cond.Broadcast()
	return uint32(len(s.calls)), nil
}

// Calls returns the notifications received so far.
func (s *Server) Calls() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Notification(nil), s.calls...)
}

// Wait blocks until n notifications arrived or the timeout expires.
func (s *Server) Wait(n int, timeout time.Duration) []Notification {
	t := time.AfterFunc(timeout, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer t.Stop()
	deadline := time.Now().Add(timeout)
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.calls) < n && time.Now().Before(deadline) {
		s.cond.Wait()
	}
	return append([]Notification(nil), s.calls...)
}

// StartNotificationServer owns org.freedesktop.Notifications on the bus at
// addr and records every Notify call.
func StartNotificationServer(t testing.TB, addr string) *Server {
	t.Helper()
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	s := &Server{}
	s.cond = sync.NewCond(&s.mu)
	if err := conn.Export(s, "/org/freedesktop/Notifications", "org.freedesktop.Notifications"); err != nil {
		t.Fatal(err)
	}
	reply, err := conn.RequestName("org.freedesktop.Notifications", dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName: %v %v", reply, err)
	}
	return s
}
