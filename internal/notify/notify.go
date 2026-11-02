// Package notify sends the end-of-timer desktop notification over D-Bus
// (org.freedesktop.Notifications).
package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	// AppName is the application name shown by the notification server.
	AppName = "pombar"
	// Summary is the notification title.
	Summary = "Time's up"
	// ExpireTimeout is passed as expire_timeout: -1 lets the notification
	// server apply the user's configured default (Plasma's own setting).
	ExpireTimeout int32 = -1
	// CallTimeout bounds the whole D-Bus exchange so a stuck bus never
	// keeps pombar from exiting.
	CallTimeout = 5 * time.Second

	service = "org.freedesktop.Notifications"
	path    = "/org/freedesktop/Notifications"
	method  = service + ".Notify"
)

// Notifier sends the end-of-timer notification.
type Notifier interface {
	Notify(summary, body string) error
}

// DBus is a Notifier that talks to the session bus named by
// $DBUS_SESSION_BUS_ADDRESS (or $XDG_RUNTIME_DIR/bus). It never starts a
// bus with dbus-launch.
type DBus struct{}

// Notify sends one notification and returns the server's error, if any.
func (DBus) Notify(summary, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	conn, err := dbus.SessionBusPrivateNoAutoStartup(dbus.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("connect to the session bus: %w", err)
	}
	defer conn.Close()
	if err := conn.Auth(nil); err != nil {
		return fmt.Errorf("authenticate to the session bus: %w", err)
	}
	if err := conn.Hello(); err != nil {
		return fmt.Errorf("session bus hello: %w", err)
	}
	hints := map[string]dbus.Variant{
		"urgency": dbus.MakeVariant(byte(1)), // normal
	}
	call := conn.Object(service, path).CallWithContext(ctx, method, 0,
		AppName, uint32(0), "", summary, body, []string{}, hints, ExpireTimeout)
	if call.Err != nil {
		return fmt.Errorf("%s: %w", method, call.Err)
	}
	return nil
}
