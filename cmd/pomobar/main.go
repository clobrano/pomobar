// Command pomobar draws a shrinking pomodoro countdown bar on a screen edge.
package main

import (
	"errors"
	"flag"
	"fmt"
	"image/color"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/clobrano/pomobar/internal/bar"
	"github.com/clobrano/pomobar/internal/notify"
	"github.com/clobrano/pomobar/internal/timer"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

// buildVersion returns the injected version or, for `go install
// …@vX.Y.Z` builds, the module version recorded by the go command.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

// Exit codes.
const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitSIGINT  = 128 + int(syscall.SIGINT)  // 130
	exitSIGTERM = 128 + int(syscall.SIGTERM) // 143
)

const usageText = `Usage: pomobar [flags] <duration>

Draw a thin bar along a screen edge that shrinks as <duration> runs out.
The bar turns amber below 50 % and red below 20 %. When the time is up
pomobar sends a desktop notification, removes the bar and exits.

<duration> uses Go syntax: 25m, 1h30m, 90s, 2m30s.

Flags:
  -edge string       screen edge: top, bottom, left or right (default "bottom")
  -thickness int     bar thickness in logical pixels (default 4)
  -reserve           reserve the bar's strip so windows do not cover it
  -label string      text shown in the notification body
  -quiet             do not send a notification at the end
  -version           print the version and exit

Examples:
  pomobar 25m
  pomobar -label "Write the report" 50m
  pomobar -edge top -thickness 6 -reserve 25m
  pomobar -quiet 5m

Exit status: 0 when the timer completes, 1 on errors (no Wayland session,
compositor without wlr-layer-shell, bar closed by the compositor), 2 on
invalid usage, 130 on SIGINT (Ctrl-C), 143 on SIGTERM. A cancelled timer
sends no notification.
`

// config is the parsed command line.
type config struct {
	duration  time.Duration
	edge      bar.Edge
	thickness int
	reserve   bool
	label     string
	quiet     bool
	version   bool
}

var errHelp = errors.New("help requested")

// parseArgs parses the command line (without the program name). Flags may
// appear before or after the duration.
func parseArgs(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("pomobar", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	edge := fs.String("edge", "bottom", "")
	fs.IntVar(&cfg.thickness, "thickness", 4, "")
	fs.BoolVar(&cfg.reserve, "reserve", false, "")
	fs.StringVar(&cfg.label, "label", "", "")
	fs.BoolVar(&cfg.quiet, "quiet", false, "")
	fs.BoolVar(&cfg.version, "version", false, "")

	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return cfg, errHelp
			}
			return cfg, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if cfg.version {
		return cfg, nil
	}

	var err error
	if cfg.edge, err = bar.ParseEdge(*edge); err != nil {
		return cfg, err
	}
	if cfg.thickness < 1 {
		return cfg, fmt.Errorf("invalid thickness %d: must be at least 1", cfg.thickness)
	}
	switch len(positional) {
	case 0:
		return cfg, errors.New("missing duration")
	case 1:
	default:
		return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(positional[1:], " "))
	}
	cfg.duration, err = time.ParseDuration(positional[0])
	if err != nil {
		return cfg, fmt.Errorf("invalid duration %q (examples: 25m, 1h30m, 90s)", positional[0])
	}
	if cfg.duration <= 0 {
		return cfg, fmt.Errorf("invalid duration %q: must be positive", positional[0])
	}
	return cfg, nil
}

// drawer is the part of *bar.Bar that run uses.
type drawer interface {
	Draw(fraction float64, c color.RGBA) error
	Done() <-chan struct{}
	Err() error
	Close() error
}

// deps are run's side effects, replaceable in tests.
type deps struct {
	openBar  func(bar.Options) (drawer, error)
	notifier notify.Notifier
	signals  <-chan os.Signal
	now      func() time.Time
	tick     time.Duration // redraw interval
	stdout   io.Writer
	stderr   io.Writer
}

func defaultDeps() deps {
	return deps{
		openBar:  func(o bar.Options) (drawer, error) { return bar.Open(o) },
		notifier: notify.DBus{},
		now:      time.Now,
		tick:     time.Second,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
	}
}

func main() {
	d := defaultDeps()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	d.signals = sig
	os.Exit(run(os.Args[1:], d))
}

func run(args []string, d deps) int {
	cfg, err := parseArgs(args)
	if errors.Is(err, errHelp) {
		fmt.Fprint(d.stdout, usageText)
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(d.stderr, "pomobar: %v\n\n%s", err, usageText)
		return exitUsage
	}
	if cfg.version {
		fmt.Fprintln(d.stdout, "pomobar", buildVersion())
		return exitOK
	}

	t, err := timer.New(cfg.duration, d.now())
	if err != nil {
		fmt.Fprintln(d.stderr, "pomobar:", err)
		return exitUsage
	}
	b, err := d.openBar(bar.Options{Edge: cfg.edge, Thickness: cfg.thickness, Reserve: cfg.reserve})
	if err != nil {
		fmt.Fprintln(d.stderr, "pomobar:", err)
		return exitError
	}

	code := countdown(t, b, d)
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

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

// countdown redraws the bar until the timer ends or the bar goes away.
func countdown(t *timer.Timer, b drawer, d deps) int {
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
		}
		now := d.now()
		if t.Done(now) {
			return exitOK
		}
		f := t.Fraction(now)
		if err := b.Draw(f, timer.Color(f)); err != nil {
			fmt.Fprintln(d.stderr, "pomobar:", err)
			return exitError
		}
		// Wake at the next tick, or exactly at the deadline if sooner.
		// Go timers follow the monotonic clock, which stops during
		// suspend; waiting at most one tick bounds the delay after a
		// resume to one tick, and the wall-clock deadline decides.
		wake.Reset(min(d.tick, t.Remaining(now)))
	}
}
