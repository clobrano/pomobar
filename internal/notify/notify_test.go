package notify

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/clobrano/pomobar/internal/dbustest"
)

func TestNotifyOnRealSessionBus(t *testing.T) {
	addr := dbustest.StartBus(t)
	srv := dbustest.StartNotificationServer(t, addr)

	if err := (DBus{}).Notify(Summary, "Write the report"); err != nil {
		t.Fatal(err)
	}
	calls := srv.Wait(1, 5*time.Second)
	if len(calls) != 1 {
		t.Fatalf("got %d notifications, want 1", len(calls))
	}
	n := calls[0]
	if n.AppName != "pomobar" || n.Summary != "Time's up" || n.Body != "Write the report" {
		t.Errorf("app/summary/body = %q/%q/%q", n.AppName, n.Summary, n.Body)
	}
	if n.ReplacesID != 0 || n.ExpireTimeout != ExpireTimeout || len(n.Actions) != 0 {
		t.Errorf("replaces %d timeout %d actions %v", n.ReplacesID, n.ExpireTimeout, n.Actions)
	}
	if u, ok := n.Hints["urgency"]; !ok || u.Value() != byte(1) {
		t.Errorf("urgency hint = %v", n.Hints["urgency"])
	}
}

func TestNotifyNoServer(t *testing.T) {
	dbustest.StartBus(t) // nobody owns org.freedesktop.Notifications
	err := (DBus{}).Notify(Summary, "x")
	if err == nil || !strings.Contains(err.Error(), "Notify") {
		t.Fatalf("err = %v, want a Notify error", err)
	}
}

func TestNotifyNoBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+t.TempDir()+"/none")
	if err := (DBus{}).Notify(Summary, "x"); err == nil {
		t.Fatal("expected an error without a bus")
	}
}

func TestAskActionInvoked(t *testing.T) {
	addr := dbustest.StartBus(t)
	srv := dbustest.StartNotificationServer(t, addr)

	q, err := (DBus{}).Ask("Break is over", "Start cycle 2/4?", "Start cycle 2")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	calls := srv.Wait(1, 5*time.Second)
	if len(calls) != 1 {
		t.Fatalf("got %d notifications", len(calls))
	}
	n := calls[0]
	if n.Summary != "Break is over" || n.ExpireTimeout != 0 ||
		len(n.Actions) != 2 || n.Actions[0] != ActionKey || n.Actions[1] != "Start cycle 2" {
		t.Fatalf("notification %+v", n)
	}
	// Another notification's action and another key are ignored.
	_ = srv.Invoke(99, ActionKey)
	_ = srv.Invoke(1, "other")
	select {
	case <-q.Answered:
		t.Fatal("answered by an unrelated action")
	case <-time.After(200 * time.Millisecond):
	}
	if err := srv.Invoke(1, ActionKey); err != nil {
		t.Fatal(err)
	}
	select {
	case <-q.Answered:
	case <-time.After(5 * time.Second):
		t.Fatal("action not received")
	}
	q.Close()
	if c := srv.Closed(); len(c) != 0 {
		t.Fatalf("an answered question was withdrawn: %v", c)
	}
}

func TestAskCloseWithdraws(t *testing.T) {
	srv := dbustest.StartNotificationServer(t, dbustest.StartBus(t))
	q, err := (DBus{}).Ask("s", "b", "a")
	if err != nil {
		t.Fatal(err)
	}
	q.Close()
	q.Close() // idempotent
	if c := srv.Closed(); len(c) != 1 || c[0] != 1 {
		t.Fatalf("closed ids %v, want [1]", c)
	}
}

func TestAskWithoutActions(t *testing.T) {
	srv := dbustest.StartNotificationServer(t, dbustest.StartBus(t))
	srv.SetActions(false)
	if _, err := (DBus{}).Ask("s", "b", "a"); !errors.Is(err, ErrNoActions) {
		t.Fatalf("err = %v, want ErrNoActions", err)
	}
	if n := srv.Calls(); len(n) != 0 {
		t.Fatalf("sent %d notifications", len(n))
	}
}
