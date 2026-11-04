# pomobar

A pomodoro timer that is just a thin bar along one edge of your screen. The
bar shrinks as time runs out, turns amber and then red, and when the time is
up pomobar sends a desktop notification and removes the bar. It can run a
whole pomodoro — work, break, repeat — and asks before starting each new
cycle.

<!-- TODO(T4.2): demo GIF recorded on KDE Plasma -->

pomobar is a single static binary with no runtime dependencies. It draws the
bar as a [wlr-layer-shell] overlay surface, so it stays above normal and
fullscreen windows on every virtual desktop, and clicks pass through it.

## Requirements

- A Wayland session (`$WAYLAND_DISPLAY` set).
- A compositor that supports **wlr-layer-shell** (`zwlr_layer_shell_v1`):
  KDE Plasma, sway, Hyprland, river, Wayfire, labwc, niri and other
  wlroots-based compositors. GNOME/Mutter does **not** support it; pomobar
  exits with an error there.
- For notifications, a notification server on the D-Bus session bus
  (`org.freedesktop.Notifications`, provided by Plasma, mako, dunst, swaync…).
  The "Start next cycle" button needs a server that supports action buttons
  (Plasma does). Without them, or without a server at all, pomobar still
  works: start the next cycle with `pomobar next`.

## Install

Download a binary for linux/amd64 or linux/arm64 from the
[releases page](https://github.com/clobrano/pomobar/releases), check it
against `SHA256SUMS`, and put `pomobar` on your `PATH`.

Or build it with Go 1.26 or later:

```sh
CGO_ENABLED=0 go install github.com/clobrano/pomobar/cmd/pomobar@latest
```

`CGO_ENABLED=0` gives a fully static binary; without it the Go toolchain
links against the system C library when a C compiler is installed.

From a checkout: `make build` (output in `bin/pomobar`).

## Usage

```
pomobar [flags] <duration>                 start a timer in the background
pomobar [flags] <work> <break> [<cycles>]  start a pomodoro (default 1 cycle)
pomobar status                             show the running timer
pomobar pause | resume                     pause or resume the running timer
pomobar next                               start the next pomodoro cycle
pomobar stop                               cancel the running timer (no notification)
```

Starting a timer returns as soon as the bar is on screen; the timer keeps
running in a background pomobar process, which you control with the other
commands. Only one timer runs at a time: starting another while one is
running fails with a message saying how much time is left.

Durations use Go syntax: `25m`, `1h30m`, `90s`, `2m30s`. Flags can go
before or after the positional arguments.

### Pomodoro cycles

`pomobar 25m 5m 4` runs four cycles of 25 minutes of work followed by a
5-minute break:

1. **Work** — the usual green → amber → red bar.
2. **Break** — a notification says "Time for a break", and a blue bar
   counts down the break.
3. **Wait** — when the break ends, the bar becomes full and dimmed and a
   notification "Break is over" with a **Start cycle 2/4** button stays on
   screen. Nothing happens until you press it (or run `pomobar next`), so
   a cycle never starts while you are away.
4. After the last break, a "Pomodoro complete" notification and the bar
   goes away.

`pomobar status` shows where you are (`work 2/4: 12:03 left of 25:00`,
`break 2/4: …`, `waiting to start cycle 3/4`). `pause`/`resume` work in
work and break phases; `stop` cancels the whole pomodoro.

| Flag | Default | Description |
|---|---|---|
| `-edge` | `bottom` | Screen edge: `top`, `bottom`, `left` or `right`. |
| `-thickness` | `4` | Bar thickness in logical pixels (scaled on HiDPI outputs). |
| `-reserve` | off | Reserve the bar's strip so windows do not cover it (layer-shell exclusive zone). By default the bar is drawn over windows. |
| `-label` | — | Shown in notifications and by `pomobar status`. For a plain timer it is the end notification's body (default "Your <duration> timer has finished."). |
| `-quiet` | off | Send no notifications. Between pomodoro cycles, use `pomobar next`. |
| `-foreground` | off | Run the timer in this terminal and exit when it ends (for scripts; Ctrl-C cancels). |
| `-version` | — | Print the version and exit. |

The bar spans the whole edge. Horizontal bars shrink toward the left,
vertical bars shrink toward the bottom. It is green with more than 50 % of
the time left, amber from 50 % to 20 % and red below 20 %, blending over
5 % of the duration below each threshold. Pomodoro breaks are blue. While
paused the bar is drawn at half opacity; between pomodoro cycles it is full
and dimmed.

### Examples

```sh
pomobar 25m                                   # a classic pomodoro
pomobar -label "Write the report" 50m         # custom notification text
pomobar -edge top -thickness 6 -reserve 25m   # thicker bar, never covered
pomobar -quiet 5m                             # short break, no notification
pomobar 25m 5m 4                              # four pomodoro cycles
pomobar -label "Thesis" 50m 10m 3             # three long cycles

pomobar status        # 12:03 left of 25:00 — Write the report
pomobar pause         # paused with 12:03 left
pomobar resume        # resumed with 12:03 left
pomobar next          # starting cycle 3/4 (between pomodoro cycles)
pomobar stop          # stopped with 12:03 left

# work, then break, in one script
pomobar -foreground 25m && pomobar -foreground -label "Break over" 5m
```

`pomobar status` prints the remaining time as `m:ss` (or `h:mm:ss`),
`(paused)` when paused, and the label; with no timer running it prints an
error and exits 1, which makes it easy to use in a status bar.

### Exit status

| Code | Meaning |
|---|---|
| 0 | Success: the timer started (or, with `-foreground`, completed), or a client command succeeded. A failed notification is only a warning. |
| 1 | Error: no Wayland session, cannot connect, no wlr-layer-shell, the compositor closed the bar (e.g. its monitor was unplugged), a timer is already running, or no timer is running for a client command. Also a command that does not fit the timer's state: pausing a paused timer, resuming a running one, `next` when not waiting, `pause`/`resume` while waiting. |
| 2 | Invalid usage (bad duration or break, cycles not a whole number ≥ 1, unknown edge, thickness < 1, extra arguments, flags given to a client command). |
| 3 | `-foreground` only: stopped with `pomobar stop`. |
| 130 | `-foreground` only: cancelled with SIGINT (Ctrl-C). |
| 143 | `-foreground` only: cancelled with SIGTERM. |

A cancelled timer never sends a notification.

## Behaviour notes

- **Suspend.** The countdown follows the wall clock, so time spent with the
  laptop suspended counts: a 25 m timer ends 25 minutes after it started,
  within a second of resuming if the deadline passed during the suspend.
  Changing the system clock moves the deadline too.
- **Pause.** A paused timer keeps its remaining time; suspend while paused
  does not count. Resuming continues from where it stopped.
- **Redraws.** pomobar wakes once a second and only redraws when the bar
  changes by at least one pixel or its color visibly changes.
- **Control socket.** The running timer listens on
  `$XDG_RUNTIME_DIR/pomobar.sock` (private to your user). A socket left by
  a crashed timer is replaced automatically.

## Limitations

- **Single monitor.** The bar appears on the output the compositor picks
  (usually the focused one); there is no `-output` option yet.
- **One timer at a time**, per user session.
- **No long breaks.** Every break in a pomodoro has the same length.
- **Integer scaling only.** On fractionally scaled outputs the bar is
  rendered at the integer scale the compositor advertises and resampled by
  the compositor, so its edges may be slightly soft.
- Linux only.

## Development

```sh
make vet test   # go vet, go test
make lint       # gofmt (+ golangci-lint if installed)
make build-all  # static linux/amd64 and linux/arm64 binaries in bin/
make dist VERSION=v0.1.0  # release archives and SHA256SUMS in dist/
```

The tests do not need a compositor: `internal/fakewl` is a small in-process
Wayland compositor that the bar is tested against, and the notification
tests start a private `dbus-daemon` (skipped if it is not installed).
Design decisions are recorded in [`docs/decisions.md`](docs/decisions.md),
the MVP task list in [`TASKS.md`](TASKS.md).

Releases are built by `.github/workflows/release.yml` when a `v*` tag is
pushed.

## License

MIT, see [LICENSE](LICENSE). Release binaries include third-party code under
BSD and MIT-style licenses, listed in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

[wlr-layer-shell]: https://wayland.app/protocols/wlr-layer-shell-unstable-v1
