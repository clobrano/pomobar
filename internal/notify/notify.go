// Package notify sends desktop notifications over D-Bus
// (org.freedesktop.Notifications): the end-of-timer notice, and questions
// with an action button that pomobar waits on between pomodoro cycles.
package notify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	// AppName is the application name shown by the notification server.
	AppName = "pomobar"
	// Summary is the title of the end-of-timer notification.
	Summary = "Time's up"
	// ExpireTimeout is passed as expire_timeout: -1 lets the notification
	// server apply the user's configured default (Plasma's own setting).
	ExpireTimeout int32 = -1
	// CallTimeout bounds every D-Bus exchange so a stuck bus never keeps
	// pomobar from going on.
	CallTimeout = 5 * time.Second

	// ActionKey is the action key of the button of a question.
	ActionKey = "start"

	service = "org.freedesktop.Notifications"
	path    = "/org/freedesktop/Notifications"
	iface   = service
)

// ErrNoActions is returned by Ask when the notification server cannot
// show action buttons.
var ErrNoActions = errors.New("the notification server does not support action buttons")

// Notifier sends notifications.
type Notifier interface {
	// Notify shows a plain notification.
	Notify(summary, body string) error
	// Ask shows a notification with one action button labelled action,
	// which stays until it is answered or closed. Answered is closed
	// when the user presses the button. Close withdraws the question
	// (if it is still shown) and releases its resources.
	Ask(summary, body, action string) (*Question, error)
}

// Question is a notification waiting for its action button.
type Question struct {
	// Answered is closed when the user presses the action button.
	Answered <-chan struct{}
	close    func()
	once     sync.Once
}

// Close withdraws the notification and stops listening for the answer.
func (q *Question) Close() {
	if q == nil {
		return
	}
	q.once.Do(func() {
		if q.close != nil {
			q.close()
		}
	})
}

// NewQuestion builds a Question from a channel and a close function, for
// fake notifiers in tests.
func NewQuestion(answered <-chan struct{}, close func()) *Question {
	return &Question{Answered: answered, close: close}
}

// DBus is a Notifier that talks to the session bus named by
// $DBUS_SESSION_BUS_ADDRESS (or $XDG_RUNTIME_DIR/bus). It never starts a
// bus with dbus-launch.
type DBus struct{}

// dial opens a private, authenticated session-bus connection, giving up
// after CallTimeout.
func dial() (*dbus.Conn, error) {
	conn, err := dbus.SessionBusPrivateNoAutoStartup()
	if err != nil {
		return nil, fmt.Errorf("connect to the session bus: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		if err := conn.Auth(nil); err != nil {
			done <- fmt.Errorf("authenticate to the session bus: %w", err)
			return
		}
		if err := conn.Hello(); err != nil {
			done <- fmt.Errorf("session bus hello: %w", err)
			return
		}
		done <- nil
	}()
	select {
	case err = <-done:
	case <-time.After(CallTimeout):
		err = errors.New("the session bus did not answer")
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func notify(conn *dbus.Conn, summary, body string, actions []string, timeout int32) (uint32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	hints := map[string]dbus.Variant{
		"urgency": dbus.MakeVariant(byte(1)), // normal
	}
	var id uint32
	err := conn.Object(service, path).CallWithContext(ctx, iface+".Notify", 0,
		AppName, uint32(0), "", summary, body, actions, hints, timeout).Store(&id)
	if err != nil {
		return 0, fmt.Errorf("%s.Notify: %w", iface, err)
	}
	return id, nil
}

// Notify sends one notification and returns the server's error, if any.
func (DBus) Notify(summary, body string) error {
	conn, err := dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = notify(conn, summary, body, []string{}, ExpireTimeout)
	return err
}

// Ask shows a persistent notification with an action button and keeps a
// connection open to receive the ActionInvoked signal. It returns
// ErrNoActions if the server does not advertise the "actions" capability.
func (DBus) Ask(summary, body, action string) (*Question, error) {
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	var caps []string
	if err := conn.Object(service, path).CallWithContext(ctx, iface+".GetCapabilities", 0).Store(&caps); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%s.GetCapabilities: %w", iface, err)
	}
	if !slices.Contains(caps, "actions") {
		conn.Close()
		return nil, ErrNoActions
	}
	for _, member := range []string{"ActionInvoked", "NotificationClosed"} {
		if err := conn.AddMatchSignalContext(ctx, dbus.WithMatchObjectPath(path),
			dbus.WithMatchInterface(iface), dbus.WithMatchMember(member)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("subscribe to %s: %w", member, err)
		}
	}
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)

	// expire_timeout 0: the question stays until it is answered.
	id, err := notify(conn, summary, body, []string{ActionKey, action}, 0)
	if err != nil {
		conn.Close()
		return nil, err
	}

	answered := make(chan struct{})
	go func() {
		for sig := range signals {
			if sig.Name != iface+".ActionInvoked" || len(sig.Body) < 2 {
				continue
			}
			if sid, _ := sig.Body[0].(uint32); sid != id {
				continue
			}
			if key, _ := sig.Body[1].(string); key == ActionKey {
				close(answered)
				return
			}
		}
	}()
	closeFn := func() {
		ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
		defer cancel()
		select {
		case <-answered:
		default:
			// Still shown: withdraw it. Errors (already closed by the
			// user) do not matter.
			_ = conn.Object(service, path).CallWithContext(ctx, iface+".CloseNotification", 0, id).Err
		}
		conn.Close() // also closes signals, ending the goroutine
	}
	return &Question{Answered: answered, close: closeFn}, nil
}
