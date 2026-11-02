# pomobar

A pomodoro timer that is just a thin bar along one edge of your screen. The
bar shrinks as time runs out, turns amber and then red, and when the time is
up pomobar sends a desktop notification, removes the bar and exits.

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
- For the end notification, a notification server on the D-Bus session bus
  (`org.freedesktop.Notifications`, provided by Plasma, mako, dunst, swaync…).
  Without one pomobar still works and prints a warning.

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
pomobar [flags] <duration>   start a timer in the background
pomobar status               show the running timer
pomobar pause | resume       pause or resume the running timer
pomobar stop                 cancel the running timer (no notification)
```

`pomobar <duration>` returns as soon as the bar is on screen; the timer keeps
running in a background pomobar process, which you control with the other
commands. Only one timer runs at a time: starting another while one is
running fails with a message saying how much time is left.

`<duration>` uses Go syntax: `25m`, `1h30m`, `90s`, `2m30s`. Flags can go
before or after it.

| Flag | Default | Description |
|---|---|---|
| `-edge` | `bottom` | Screen edge: `top`, `bottom`, `left` or `right`. |
| `-thickness` | `4` | Bar thickness in logical pixels (scaled on HiDPI outputs). |
| `-reserve` | off | Reserve the bar's strip so windows do not cover it (layer-shell exclusive zone). By default the bar is drawn over windows. |
| `-label` | — | Notification body, also shown by `pomobar status`. Default notification body: "Your <duration> timer has finished." |
| `-quiet` | off | Do not send a notification at the end. |
| `-foreground` | off | Run the timer in this terminal and exit when it ends (for scripts; Ctrl-C cancels). |
| `-version` | — | Print the version and exit. |

The bar spans the whole edge. Horizontal bars shrink toward the left,
vertical bars shrink toward the bottom. It is green with more than 50 % of
the time left, amber from 50 % to 20 % and red below 20 %, blending over
5 % of the duration below each threshold. While paused it is drawn at half
opacity.

### Examples

```sh
pomobar 25m                                   # a classic pomodoro
pomobar -label "Write the report" 50m         # custom notification text
pomobar -edge top -thickness 6 -reserve 25m   # thicker bar, never covered
pomobar -quiet 5m                             # short break, no notification

pomobar status        # 12:03 left of 25:00 — Write the report
pomobar pause         # paused with 12:03 left
pomobar resume        # resumed with 12:03 left
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
| 1 | Error: no Wayland session, cannot connect, no wlr-layer-shell, the compositor closed the bar (e.g. its monitor was unplugged), a timer is already running, or no timer is running for `status`/`stop`/`pause`/`resume` (also pausing a paused timer or resuming a running one). |
| 2 | Invalid usage (bad duration, unknown edge, thickness < 1, extra arguments, flags given to a client command). |
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
- **One timer at a time**, per user session. There are no pomodoro cycles;
  chain `-foreground` timers in the shell instead.
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
