# Design decisions

## D1 — Wayland library: `github.com/vcaesar/go-wayland` (M1, T1.1–T1.3)

Resolves PRD open question 1.

Candidates: `github.com/vcaesar/go-wayland` and `github.com/xogas/wayland`.

| | vcaesar/go-wayland | xogas/wayland |
|---|---|---|
| Pure Go, `CGO_ENABLED=0` | yes | yes |
| Scanner | yes (`cmd/go-wayland-scanner`) | not decisive, not assessed |
| `wlr-layer-shell-unstable-v1` bindings | **shipped** (`wlr/layer-shell-v1`) | no; would need vendoring the XML and generating bindings |
| Core `wl_shm`, `wl_region`, `wl_output` | yes (`client`, wayland 1.25) | not assessed |
| Event loop | `Context.Dispatch` on one goroutine; sends are goroutine-safe except object-creating requests | not assessed |
| Minimum Go | 1.26 | 1.27 |
| Maintenance | active fork of rajveermalviya/go-wayland (v0.50.0 released 2026-10-22) | not assessed |

We use **vcaesar/go-wayland v0.50.0**. It ships layer-shell bindings, so
nothing is vendored or generated in this repository and the placeholder
`internal/protocol/layershell` package was removed (T1.3 is not needed, and
T4.5 needs no third-party notice for wlr-protocols). It also keeps our
minimum Go version at 1.26.

### Concurrency model it imposes

`client.Context` allows requests from several goroutines, **except** that
requests creating objects must be serialized (new ids must reach the
compositor in order), and reading/dispatching must happen on a single
goroutine. pomobar therefore:

- runs `Context.Dispatch` in a loop on a dedicated goroutine (event
  handlers run there);
- guards all bar state with one mutex in `bar.Bar`, and sends every request
  (object-creating or not) while holding it, from both the caller's
  goroutine (`Draw`, `Close`) and event handlers (`configure`,
  `wl_buffer.release`);
- does the initial registry/bind/configure round trips on the caller's
  goroutine before the dispatch goroutine starts.

Generated proxies store event handlers in plain fields read by `Dispatch`.
For objects created after the dispatch goroutine is running (buffers, the
teardown `wl_callback`) the handler is set and then published through the
context's object-table mutex before the request that could trigger the
event is sent.

### Teardown

`Close` destroys the layer surface, the surface and the shm buffers/pool,
then sends `wl_display.sync` and waits for its `done` event, which arrives
on the dispatch goroutine, with a 2 s timeout. Only then is the connection
closed. The `wl_callback` handler is installed before the sync request is
written (we build the request by hand because `Display.Sync` sends first
and returns the callback afterwards, which could miss a fast reply).

## D2 — Layer: `overlay`

The bar uses the `overlay` layer so it stays above fullscreen windows. If
it misbehaves on KDE Plasma, switch to `top` and record it here (PRD risk;
needs manual verification on KDE).

## D3 — Countdown on the wall clock (M2)

`timer.Timer` strips Go's monotonic reading (`time.Now().Round(0)`) from
its start and from every `now`, so all arithmetic uses the wall clock. On
Linux Go's monotonic clock is `CLOCK_MONOTONIC`, which stops while the
system is suspended: a monotonic deadline would end late by the length of
the suspend. The wall clock keeps running, so a 25 m timer ends 25
wall-clock minutes after it started even across a suspend.

The main loop sleeps with Go timers (which are monotonic) for at most one
second at a time, so after a resume the next wake-up is at most one second
away and sees that the wall-clock deadline has passed. Without a suspend it
wakes exactly at the deadline.

Trade-off: a manual clock change or an NTP step moves the deadline by the
same amount.

## D4 — Notification

`org.freedesktop.Notifications.Notify` over a private session-bus
connection (`godbus/dbus/v5`, never auto-launching a bus): app name
`pomobar`, summary `Time's up`, body = `-label` or "Your <duration> timer
has finished.", urgency normal, `expire_timeout = -1` so the notification
server applies the user's own default (Plasma's notification settings).
The whole exchange is bounded by a 5 s timeout. A failure prints one
warning line on stderr and never changes the exit status.

The bar is removed before the notification is sent, so it disappears at
once even if the bus is slow.

## D5 — Exit codes

| Situation | Exit | Notification |
|---|---|---|
| Timer completed | 0 | yes, unless `-quiet` |
| `-version`, `-h` | 0 | no |
| No `$WAYLAND_DISPLAY`, connection failure, no layer-shell, protocol error | 1 | no |
| Compositor sent `closed` for the bar (e.g. its output went away) | 1 | no |
| Invalid usage | 2 | no |
| `-foreground` timer stopped with `pomobar stop` | 3 | no |
| SIGINT (Ctrl-C) | 130 | no |
| SIGTERM | 143 | no |

`closed` exits 1 because the timer did not complete and pomobar cannot show
it any more; the user should notice rather than mistake it for success.

## D6 — Background timer and client commands

`pomobar <duration>` no longer blocks the terminal. It validates the
arguments, refuses if a timer is already running, and re-executes itself
as `pomobar -foreground <args>` in a new session (`setsid`, stdio on
`/dev/null`, cwd `/`). The child reports on an inherited pipe (fd 3,
named by `POMOBAR_READY_FD`) either `OK` once its control socket is
listening and the bar is mapped, or `ERR <message>`; the parent prints the
message and exits 1, or exits 0. Errors such as a missing layer-shell are
therefore still reported in the terminal that started the timer.

The timer process listens on `$XDG_RUNTIME_DIR/pomobar.sock`. Each
connection carries one JSON request line (`{"cmd":"status"}`) and one JSON
response line with `ok`, `error` and the timer `state` (duration,
remaining, paused, label). Requests are handed to the countdown loop over
a channel, so the timer state is only touched by that goroutine.

- **One timer at a time.** Claiming the socket is the lock: if a live
  timer answers on it, a new one refuses with its status ("a timer is
  already running (12:03 left of 25:00); use "pomobar stop" first"). A
  socket nobody answers on is stale and is replaced.
- **Commands:** `status`, `stop` (no notification), `pause`, `resume`.
  Pausing freezes the remaining time; resuming sets a new wall-clock
  deadline of now + remaining, so suspend while paused does not count. The
  bar is drawn at half opacity while paused.
- **`-foreground`** keeps the old blocking behaviour for scripts; that
  process also serves the control socket, and exits 3 when stopped by a
  client.
- The detached process has no terminal, so notification warnings are not
  shown anywhere. Logging to the journal is possible future work.
