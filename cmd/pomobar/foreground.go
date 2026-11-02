package main

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/clobrano/pomobar/internal/bar"
	"github.com/clobrano/pomobar/internal/ipc"
	"github.com/clobrano/pomobar/internal/notify"
	"github.com/clobrano/pomobar/internal/timer"
)

// request is a client request handed from the control socket to the
// countdown loop.
type request struct {
	req   ipc.Request
	reply chan ipc.Response
}

// runTimer runs one timer in this process: it claims the control socket,
// shows the bar, counts down while answering client commands, and
// notifies at the end.
func runTimer(cfg config, d deps) int {
	fail := func(err error) int {
		reportReady(d, err)
		fmt.Fprintln(d.stderr, "pomobar:", err)
		return exitError
	}

	path, err := ipc.SocketPath()
	if err != nil {
		return fail(err)
	}
	ln, err := ipc.Listen(path)
	if errors.Is(err, ipc.ErrRunning) {
		return fail(alreadyRunning(path))
	}
	if err != nil {
		return fail(fmt.Errorf("control socket %s: %w", path, err))
	}
	defer ln.Close() // also removes the socket file

	t, err := timer.New(cfg.duration, d.now())
	if err != nil {
		return fail(err)
	}
	b, err := d.openBar(bar.Options{Edge: cfg.edge, Thickness: cfg.thickness, Reserve: cfg.reserve})
	if err != nil {
		return fail(err)
	}

	reqs := make(chan request)
	loopDone := make(chan struct{})
	served := make(chan struct{})
	go func() {
		defer close(served)
		ipc.Serve(ln, func(r ipc.Request) ipc.Response {
			rq := request{req: r, reply: make(chan ipc.Response, 1)}
			select {
			case reqs <- rq:
			case <-loopDone:
				return ipc.Response{Error: "the timer is ending"}
			}
			select {
			case resp := <-rq.reply:
				return resp
			case <-loopDone:
				return ipc.Response{Error: "the timer is ending"}
			}
		})
	}()
	// Show the first frame and wait until the compositor has it, so the
	// bar is on screen when a detached start returns.
	f := t.Fraction(d.now())
	if err := b.Draw(f, timer.Color(f)); err != nil {
		_ = b.Close()
		return fail(err)
	}
	if err := b.Sync(); err != nil {
		_ = b.Close()
		return fail(err)
	}
	reportReady(d, nil)

	code := countdown(t, b, reqs, cfg, d)
	close(loopDone)
	ln.Close() // `pomobar status` reports "not running" from now on
	<-served   // let in-flight replies (e.g. to "stop") reach their clients

	// Remove the bar first, so it disappears at once whatever happens
	// next. Close waits for the compositor to process the teardown.
	if err := b.Close(); err != nil && code == exitOK {
		fmt.Fprintln(d.stderr, "pomobar: warning: closing the Wayland connection:", err)
	}
	if code == exitOK && !cfg.quiet {
		if err := d.notifier.Notify(notify.Summary, notificationBody(cfg)); err != nil {
			// A missing notification never changes the exit status.
			fmt.Fprintln(d.stderr, "pomobar: warning: notification failed:", oneLine(err))
		}
	}
	return code
}

// notificationBody is the label, or a default naming the duration.
func notificationBody(cfg config) string {
	if cfg.label != "" {
		return cfg.label
	}
	return fmt.Sprintf("Your %s timer has finished.", cfg.duration)
}

// countdown redraws the bar and answers client requests until the timer
// ends, is cancelled, or the bar goes away.
func countdown(t *timer.Timer, b drawer, reqs <-chan request, cfg config, d deps) int {
	wake := time.NewTimer(0)
	defer wake.Stop()
	for {
		select {
		case <-wake.C:
		case <-b.Done():
			fmt.Fprintln(d.stderr, "pomobar:", b.Err())
			return exitError
		case s := <-d.signals:
			if s == syscall.SIGTERM {
				return exitSIGTERM
			}
			return exitSIGINT
		case r := <-reqs:
			if stop := handle(t, r, cfg, d.now()); stop {
				return exitStopped
			}
		}
		now := d.now()
		if t.Done(now) {
			return exitOK
		}
		f := t.Fraction(now)
		c := timer.Color(f)
		if t.Paused() {
			c = timer.Dim(c)
		}
		if err := b.Draw(f, c); err != nil {
			fmt.Fprintln(d.stderr, "pomobar:", err)
			return exitError
		}
		// Wake at the next tick, or exactly at the deadline if sooner.
		// Go timers follow the monotonic clock, which stops during
		// suspend; waiting at most one tick bounds the delay after a
		// resume to one tick, and the wall-clock deadline decides.
		next := d.tick
		if !t.Paused() {
			next = min(next, t.Remaining(now))
		}
		wake.Reset(next) // Go >= 1.23: Reset drops a fired, unread tick

	}
}

// handle answers one client request; it reports whether the timer must
// stop.
func handle(t *timer.Timer, r request, cfg config, now time.Time) (stop bool) {
	resp := ipc.Response{OK: true}
	switch r.req.Cmd {
	case ipc.CmdStatus:
	case ipc.CmdStop:
		stop = true
	case ipc.CmdPause:
		if !t.Pause(now) {
			resp = ipc.Response{Error: "the timer is already paused"}
		}
	case ipc.CmdResume:
		if !t.Resume(now) {
			resp = ipc.Response{Error: "the timer is not paused"}
		}
	default:
		resp = ipc.Response{Error: fmt.Sprintf("unknown command %q", r.req.Cmd)}
	}
	resp.State = ipc.State{
		Duration:  t.Duration(),
		Remaining: t.Remaining(now),
		Paused:    t.Paused(),
		Label:     cfg.label,
	}
	r.reply <- resp
	return stop
}
