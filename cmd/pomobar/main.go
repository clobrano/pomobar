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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/clobrano/pomobar/internal/bar"
	"github.com/clobrano/pomobar/internal/ipc"
	"github.com/clobrano/pomobar/internal/notify"
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
	exitStopped = 3                          // a -foreground timer was stopped with `pomobar stop`
	exitSIGINT  = 128 + int(syscall.SIGINT)  // 130
	exitSIGTERM = 128 + int(syscall.SIGTERM) // 143
)

const usageText = `Usage:
  pomobar [flags] <duration>                 start a timer in the background
  pomobar [flags] <work> <break> [<cycles>]  start a pomodoro (default 1 cycle)
  pomobar status                             show the running timer
  pomobar pause | resume                     pause or resume the running timer
  pomobar next                               start the next pomodoro cycle
  pomobar stop                               cancel the running timer

Draw a thin bar along a screen edge that shrinks as the time runs out.
Work bars turn amber below 50 % and red below 20 %; break bars are blue;
paused bars are dimmed. When the time is up pomobar sends a desktop
notification, removes the bar and exits. Only one timer runs at a time.

A pomodoro repeats <cycles> times: <work>, then <break>. After each break
pomobar shows a full dimmed bar and waits: the next cycle starts when you
press "Start" in the notification or run "pomobar next".

Durations use Go syntax: 25m, 1h30m, 90s, 2m30s.

Flags:
  -edge string       screen edge: top, bottom, left or right (default "bottom")
  -thickness int     bar thickness in logical pixels (default 4)
  -reserve           reserve the bar's strip so windows do not cover it
  -label string      text shown in notifications and in status
  -quiet             send no notifications (use "pomobar next" between cycles)
  -foreground        run the timer in this terminal until it ends
  -version           print the version and exit

Examples:
  pomobar 25m
  pomobar 25m 5m 4
  pomobar -label "Write the report" 50m 10m 2
  pomobar -edge top -thickness 6 -reserve 25m
  pomobar status
  pomobar -foreground 25m && pomobar -foreground -label "Break over" 5m

Exit status: 0 on success; 1 on errors (no Wayland session, compositor
without wlr-layer-shell, bar closed by the compositor, a timer already
running, no timer running for a client command); 2 on invalid usage.
With -foreground the process exits when the timer does: 0 when it
completes, 3 when stopped with "pomobar stop", 130 on SIGINT (Ctrl-C),
143 on SIGTERM. A cancelled timer sends no notification.
`

// config is the parsed command line.
type config struct {
	duration   time.Duration
	edge       bar.Edge
	thickness  int
	reserve    bool
	label      string
	quiet      bool
	foreground bool
	version    bool
	// rest and cycles describe a pomodoro: cycles × (duration of work +
	// rest). cycles is 0 for a plain timer.
	rest   time.Duration
	cycles int
	// cmd is a client command (status, stop, pause, resume, next), or ""
	// to start a timer.
	cmd string
}

// pomodoro reports whether the timer has break phases and cycles.
func (c config) pomodoro() bool { return c.cycles > 0 }

// clientCommands are the positional words that talk to a running timer.
var clientCommands = map[string]bool{
	ipc.CmdStatus: true, ipc.CmdStop: true, ipc.CmdPause: true, ipc.CmdResume: true,
	ipc.CmdNext: true,
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
	fs.BoolVar(&cfg.foreground, "foreground", false, "")
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

	if len(positional) > 0 && clientCommands[positional[0]] {
		cfg.cmd = positional[0]
		if len(positional) > 1 {
			return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(positional[1:], " "))
		}
		var set []string
		fs.Visit(func(f *flag.Flag) { set = append(set, "-"+f.Name) })
		if len(set) > 0 {
			return cfg, fmt.Errorf("%s takes no flags (got %s)", cfg.cmd, strings.Join(set, " "))
		}
		return cfg, nil
	}

	var err error
	if cfg.edge, err = bar.ParseEdge(*edge); err != nil {
		return cfg, err
	}
	if cfg.thickness < 1 {
		return cfg, fmt.Errorf("invalid thickness %d: must be at least 1", cfg.thickness)
	}
	if len(positional) == 0 {
		return cfg, errors.New("missing duration or command")
	}
	if len(positional) > 3 {
		return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(positional[3:], " "))
	}
	if cfg.duration, err = parseDuration(positional[0], "duration"); err != nil {
		return cfg, err
	}
	if len(positional) >= 2 {
		if cfg.rest, err = parseDuration(positional[1], "break"); err != nil {
			return cfg, err
		}
		cfg.cycles = 1
	}
	if len(positional) == 3 {
		n, err := strconv.Atoi(positional[2])
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("invalid number of cycles %q: must be a whole number of at least 1", positional[2])
		}
		cfg.cycles = n
	}
	return cfg, nil
}

func parseDuration(arg, what string) (time.Duration, error) {
	d, err := time.ParseDuration(arg)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q (examples: 25m, 1h30m, 90s)", what, arg)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid %s %q: must be positive", what, arg)
	}
	return d, nil
}

// drawer is the part of *bar.Bar that run uses.
type drawer interface {
	Draw(fraction float64, c color.RGBA) error
	Sync() error
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
	// spawn starts `pomobar -foreground args...` detached from the
	// terminal and returns once it is running or has failed.
	spawn func(args []string) error
	// ready, if set, is told whether a -foreground timer started; the
	// parent of a detached timer waits on it.
	ready io.WriteCloser
}

func defaultDeps() deps {
	return deps{
		openBar:  func(o bar.Options) (drawer, error) { return bar.Open(o) },
		notifier: notify.DBus{},
		now:      time.Now,
		tick:     time.Second,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
		spawn:    spawnDetached,
	}
}

func main() {
	d := defaultDeps()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	d.signals = sig
	d.ready = readyPipeFromEnv()
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
	switch {
	case cfg.version:
		fmt.Fprintln(d.stdout, "pomobar", buildVersion())
		return exitOK
	case cfg.cmd != "":
		return runClient(cfg.cmd, d)
	case cfg.foreground:
		return runTimer(cfg, d)
	default:
		return startDetached(args, d)
	}
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}
