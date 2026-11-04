package main

import (
	"errors"
	"fmt"
	"image/color"
	"os"
	"syscall"
	"time"

	"github.com/clobrano/pomobar/internal/bar"
	"github.com/clobrano/pomobar/internal/ipc"
	"github.com/clobrano/pomobar/internal/notify"
	"github.com/clobrano/pomobar/internal/timer"
)

// request is a client request handed from the control socket to the
// timer loop.
type request struct {
	req   ipc.Request
	reply chan ipc.Response
}

// runTimer runs one timer (or pomodoro sequence) in this process: it
// claims the control socket, shows the bar, counts down while answering
// client commands, and notifies at the end.
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

	s := &session{cfg: cfg, d: d, b: b, reqs: reqs, t: t, phase: ipc.PhaseWork, cycle: 1}
	// Show the first frame and wait until the compositor has it, so the
	// bar is on screen when a detached start returns.
	if err := s.draw(d.now()); err != nil {
		_ = b.Close()
		return fail(err)
	}
	if err := b.Sync(); err != nil {
		_ = b.Close()
		return fail(err)
	}
	reportReady(d, nil)

	code := s.run()
	close(loopDone)
	ln.Close() // `pomobar status` reports "not running" from now on
	<-served   // let in-flight replies (e.g. to "stop") reach their clients

	// Remove the bar first, so it disappears at once whatever happens
	// next. Close waits for the compositor to process the teardown.
	if err := b.Close(); err != nil && code == exitOK {
		fmt.Fprintln(d.stderr, "pomobar: warning: closing the Wayland connection:", err)
	}
	if code == exitOK {
		summary, body := notify.Summary, notificationBody(cfg)
		if cfg.pomodoro() {
			summary = "Pomodoro complete"
			body = labelPrefix(cfg) + fmt.Sprintf("All %d cycles done.", cfg.cycles)
			if cfg.cycles == 1 {
				body = labelPrefix(cfg) + "Cycle done."
			}
		}
		s.notify(summary, body)
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

// labelPrefix is "label: " or "".
func labelPrefix(cfg config) string {
	if cfg.label == "" {
		return ""
	}
	return cfg.label + ": "
}

// session is the state of a running timer, owned by the loop goroutine.
type session struct {
	cfg  config
	d    deps
	b    drawer
	reqs <-chan request

	phase string       // ipc.Phase*
	t     *timer.Timer // current countdown; nil while waiting
	cycle int          // 1-based
}

// run counts down the work phase and, for a pomodoro, the break and the
// following cycles, each started only after the user's approval.
func (s *session) run() int {
	for {
		if code, ok := s.countdown(); !ok {
			return code
		}
		if !s.cfg.pomodoro() {
			return exitOK
		}
		s.notify("Time for a break", labelPrefix(s.cfg)+
			fmt.Sprintf("Cycle %d/%d done: take a %s break.", s.cycle, s.cfg.cycles, s.cfg.rest))
		s.start(ipc.PhaseBreak, s.cfg.rest)
		if code, ok := s.countdown(); !ok {
			return code
		}
		if s.cycle == s.cfg.cycles {
			return exitOK
		}
		s.cycle++
		if code, ok := s.waitApproval(); !ok {
			return code
		}
		s.start(ipc.PhaseWork, s.cfg.duration)
	}
}

func (s *session) start(phase string, d time.Duration) {
	s.phase = phase
	s.t, _ = timer.New(d, s.d.now()) // d > 0, checked by parseArgs
}

// notify sends a notification unless -quiet; failures only warn.
func (s *session) notify(summary, body string) {
	if s.cfg.quiet {
		return
	}
	if err := s.d.notifier.Notify(summary, body); err != nil {
		fmt.Fprintln(s.d.stderr, "pomobar: warning: notification failed:", oneLine(err))
	}
}

// color is the bar color for the current state.
func (s *session) color(f float64) color.RGBA {
	var c color.RGBA
	switch s.phase {
	case ipc.PhaseBreak:
		c = timer.Blue
	case ipc.PhaseWaiting:
		return timer.Dim(timer.Green)
	default:
		c = timer.Color(f)
	}
	if s.t.Paused() {
		c = timer.Dim(c)
	}
	return c
}

// draw renders the current state at now.
func (s *session) draw(now time.Time) error {
	f := 1.0
	if s.t != nil {
		f = s.t.Fraction(now)
	}
	return s.b.Draw(f, s.color(f))
}

// exitForSignal is the exit code for a cancelling signal.
func exitForSignal(sig os.Signal) int {
	if sig == syscall.SIGTERM {
		return exitSIGTERM
	}
	return exitSIGINT
}

// countdown runs the current phase. It returns ok when the phase
// completed, otherwise the exit code.
func (s *session) countdown() (code int, ok bool) {
	wake := time.NewTimer(0)
	defer wake.Stop()
	for {
		select {
		case <-wake.C:
		case <-s.b.Done():
			fmt.Fprintln(s.d.stderr, "pomobar:", s.b.Err())
			return exitError, false
		case sig := <-s.d.signals:
			return exitForSignal(sig), false
		case r := <-s.reqs:
			if stop := s.handle(r); stop {
				return exitStopped, false
			}
		}
		now := s.d.now()
		if s.t.Done(now) {
			return exitOK, true
		}
		if err := s.draw(now); err != nil {
			fmt.Fprintln(s.d.stderr, "pomobar:", err)
			return exitError, false
		}
		// Wake at the next tick, or exactly at the deadline if sooner.
		// Go timers follow the monotonic clock, which stops during
		// suspend; waiting at most one tick bounds the delay after a
		// resume to one tick, and the wall-clock deadline decides.
		next := s.d.tick
		if !s.t.Paused() {
			next = min(next, s.t.Remaining(now))
		}
		wake.Reset(next) // Go >= 1.23: Reset drops a fired, unread tick
	}
}

// waitApproval shows a full, dimmed bar and waits until the user starts
// the next cycle: with the notification's action button, or with
// `pomobar next` (always possible, and the only way with -quiet or a
// notification server without action buttons).
func (s *session) waitApproval() (code int, ok bool) {
	s.phase, s.t = ipc.PhaseWaiting, nil
	if err := s.draw(time.Time{}); err != nil {
		fmt.Fprintln(s.d.stderr, "pomobar:", err)
		return exitError, false
	}

	var answered <-chan struct{}
	if !s.cfg.quiet {
		const summary = "Break is over"
		next := fmt.Sprintf("cycle %d/%d", s.cycle, s.cfg.cycles)
		q, err := s.d.notifier.Ask(summary,
			labelPrefix(s.cfg)+"Ready for "+next+"? Start it here or run \"pomobar next\".",
			"Start "+next)
		switch {
		case errors.Is(err, notify.ErrNoActions):
			s.notify(summary, labelPrefix(s.cfg)+"Run \"pomobar next\" to start "+next+".")
		case err != nil:
			fmt.Fprintln(s.d.stderr, "pomobar: warning: notification failed:", oneLine(err))
		default:
			answered = q.Answered
			defer q.Close() // withdraws it if approved with `pomobar next`
		}
	}

	for {
		select {
		case <-answered:
			return exitOK, true
		case <-s.b.Done():
			fmt.Fprintln(s.d.stderr, "pomobar:", s.b.Err())
			return exitError, false
		case sig := <-s.d.signals:
			return exitForSignal(sig), false
		case r := <-s.reqs:
			switch r.req.Cmd {
			case ipc.CmdNext:
				s.reply(r, ipc.Response{OK: true})
				return exitOK, true
			default:
				if stop := s.handle(r); stop {
					return exitStopped, false
				}
			}
		}
	}
}

// handle answers one client request; it reports whether the timer must
// stop.
func (s *session) handle(r request) (stop bool) {
	now := s.d.now()
	resp := ipc.Response{OK: true}
	waiting := s.phase == ipc.PhaseWaiting
	switch r.req.Cmd {
	case ipc.CmdStatus:
	case ipc.CmdStop:
		stop = true
	case ipc.CmdPause, ipc.CmdResume:
		switch {
		case waiting:
			resp = ipc.Response{Error: `the timer is waiting to start the next cycle; run "pomobar next"`}
		case r.req.Cmd == ipc.CmdPause && !s.t.Pause(now):
			resp = ipc.Response{Error: "the timer is already paused"}
		case r.req.Cmd == ipc.CmdResume && !s.t.Resume(now):
			resp = ipc.Response{Error: "the timer is not paused"}
		}
	case ipc.CmdNext:
		resp = ipc.Response{Error: "the timer is not waiting to start a cycle"}
	default:
		resp = ipc.Response{Error: fmt.Sprintf("unknown command %q", r.req.Cmd)}
	}
	s.reply(r, resp)
	return stop
}

// reply sends resp with the current state.
func (s *session) reply(r request, resp ipc.Response) {
	st := ipc.State{Phase: s.phase, Label: s.cfg.label}
	if s.t != nil {
		now := s.d.now()
		st.Duration, st.Remaining, st.Paused = s.t.Duration(), s.t.Remaining(now), s.t.Paused()
	}
	if s.cfg.pomodoro() {
		st.Cycle, st.Cycles = s.cycle, s.cfg.cycles
	}
	resp.State = st
	r.reply <- resp
}
