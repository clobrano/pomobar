package notify

import (
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
