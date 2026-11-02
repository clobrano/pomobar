# pomobar — Task list

Derived from the pombar PRD (Oct 26, 2026). Tasks are grouped by milestone, in
the order they should be done. Each milestone ends with its acceptance
criteria as a checklist; the MVP is done when M1–M4 all pass on KDE Plasma
Wayland.

> Naming: the PRD called the binary `pombar`; the final name is `pomobar`
> everywhere (module path, binary, D-Bus app name, layer-shell namespace).

## M0 — Project bootstrap

- [x] **T0.1** Initialise the Go module (`go mod init github.com/clobrano/pomobar`), pick a Go version, add `.gitignore` entries for build output.
- [x] **T0.2** Create the package skeleton: `cmd/pomobar`, `internal/timer`, `internal/bar`, `internal/notify`, `internal/protocol/layershell` (placeholder).
- [x] **T0.3** Add a `Makefile` (or `justfile`) with `build` (`CGO_ENABLED=0`), `test`, `vet`, `lint` targets.
- [x] **T0.4** CI workflow (GitHub Actions): `go vet`, `go test ./...`, and a `CGO_ENABLED=0` build for linux/amd64 and linux/arm64 on every push/PR.
- [x] **T0.5** Inject version via `-ldflags "-X main.version=…"` so `-version` has something to print later.

## M1 — Layer-shell spike

Goal: a hard-coded binary that draws a 4 px green bar on the bottom edge for
60 s, shrinking, then exits.

### Library decision
- [x] **T1.1** Evaluate `vcaesar/go-wayland` vs `xogas/wayland`: maintenance status, pure-Go (no cgo), scanner availability, `wl_shm`/`wl_region`/`wl_output` coverage, event-loop model. Record the decision and reasons in `docs/decisions.md` (resolves PRD open question 1).
- [x] **T1.2** Check whether the chosen library ships `zwlr_layer_shell_v1` bindings.
- [x] **T1.3** ~~If not: vendor `wlr-layer-shell-unstable-v1.xml` from wlr-protocols (with its licence), generate bindings with the library's scanner into `internal/protocol/layershell`, and add a `go generate` directive so they can be regenerated.~~ Not needed: go-wayland ships `wlr/layer-shell-v1`; the placeholder package was removed.

### Wayland plumbing (`internal/bar`)
- [x] **T1.4** Connect to `$WAYLAND_DISPLAY`; return a typed error if the variable is unset or the connection fails.
- [x] **T1.5** Registry: bind `wl_compositor`, `wl_shm`, `zwlr_layer_shell_v1` (and `wl_output` for scale). Return a distinct "layer-shell not supported" error if the global is missing.
- [x] **T1.6** Create `wl_surface`; call `get_layer_surface` with no output, layer `overlay`, namespace `pomobar`.
- [x] **T1.7** Anchor bottom + left + right, size `(0, 4)`, exclusive zone `0`, keyboard interactivity `none`.
- [x] **T1.8** Set an empty `wl_region` as the input region (click-through).
- [x] **T1.9** Initial commit → wait for `configure` → `ack_configure`; handle later `configure` events (size changes) and `closed`.
- [x] **T1.10** shm buffer pool: `unix.MemfdCreate` + `ftruncate` + `mmap`, ARGB8888, two buffers; track `wl_buffer.release` so a busy buffer is never written.
- [x] **T1.11** Draw function: fill the first `length × fraction` pixels with a color, the rest with transparent zero (premultiplied ARGB).
- [x] **T1.12** Run the Wayland event loop on its own goroutine; expose a safe way for the main goroutine to request a redraw and to shut down.
- [x] **T1.13** Clean teardown: destroy layer surface, surface, buffers, unmap memory, close the connection.

### Spike binary
- [x] **T1.14** Temporary `cmd/pomobar` main: fixed 60 s, green, bottom edge, redraw each second, exit 0.

### M1 acceptance (manual, on KDE Plasma Wayland)

Automated coverage: `internal/bar` tests run the client against the fake
compositor in `internal/fakewl` (overlay layer, anchors, size, empty input
region, ack before attach, pixels, buffer reuse, resize, `closed`,
teardown). The KDE items below still need manual verification on KDE Plasma.

- [ ] Bar is visible above normal windows and above fullscreen apps.
- [ ] Bar is visible on every virtual desktop.
- [ ] Clicks pass through the bar to the window beneath.
- [x] `CGO_ENABLED=0 go build ./...` succeeds; `ldd` reports a static binary (verified for linux/amd64; `file` confirms linux/arm64 is statically linked).
- [ ] If overlay misbehaves on KDE, switch to the `top` layer and note it in `docs/decisions.md` (PRD risk).

## M2 — CLI and timer

### `internal/timer` (pure, no Wayland)
- [x] **T2.1** `Timer` type storing start and absolute deadline from `time.Now()` ~~(monotonic)~~ — uses the wall clock (`time.Now().Round(0)`), see the `timer.Timer` doc comment; `Remaining(now)` and `Fraction(now)` clamped to `[0, 1]`; `Done(now)`.
- [x] **T2.2** `Color(fraction)`: green `#4CAF50` above 50 %, amber `#FFB300` 50–20 %, red `#E53935` below 20 %, with smooth interpolation across each boundary. Define the exact blend width in a constant and document it.
- [x] **T2.3** `FilledLength(fraction, length)` helper (integer pixels) used for "changed by ≥ 1 px" checks.
- [x] **T2.4** Unit tests: fraction at start/middle/end/past deadline, zero/negative durations rejected, color at and around each threshold, interpolation monotonicity, filled-length rounding.

### CLI (`cmd/pomobar`)
- [x] **T2.5** Parse flags with the standard `flag` package: `-edge`, `-thickness`, `-reserve`, `-label`, `-quiet`, `-version`; positional `<duration>` via `time.ParseDuration`.
- [x] **T2.6** Validate: missing/extra args, non-positive duration, unknown edge, thickness < 1. Print usage and exit 2 on invalid usage.
- [x] **T2.7** Usage text matching the PRD examples.

### Bar options (`internal/bar`)
- [x] **T2.8** Support all four edges: anchor edge + its two neighbours; size `(0, t)` for top/bottom, `(t, 0)` for left/right.
- [x] **T2.9** Shrink direction: horizontal bars shrink toward the left, vertical bars toward the bottom (fill from the bottom up).
- [x] **T2.10** `-reserve`: exclusive zone = thickness, otherwise 0.
- [x] **T2.11** HiDPI: read integer scale from `wl_output`, call `wl_surface.set_buffer_scale`, allocate buffers at `size × scale`.
- [x] **T2.12** Accept a color per frame from the caller (replace hard-coded green).

### Main loop
- [x] **T2.13** 1 s ticker computes fraction and color from the ~~monotonic~~ wall-clock deadline; only attach/commit when filled length changes by ≥ 1 px or the color changes visibly (`timer.ColorStep`: any 8-bit channel moves by ≥ 2).
- [x] **T2.14** When remaining ≤ 0, stop ticking and proceed to the end-of-timer path.

### M2 acceptance

The countdown uses the wall clock: Go's monotonic clock stops while a Linux
laptop is suspended, so a monotonic deadline would end late by the length of
the suspend. The main loop wakes at least once per second (Go timers are
monotonic), so after a resume the timer ends within one tick.

- [ ] All four edges render correctly and shrink in the documented direction. *(Verified against the fake compositor — anchors, sizes, pixels — in `internal/bar`; needs an on-screen check on KDE Plasma.)*
- [x] A 25 m timer ends within 1 s of wall-clock time. *(Real 25 m run against the fake compositor with `POMOBAR_LONG_RUN=25m go test -run TestLongRun ./cmd/pomobar`: ended 0.6 ms after the wall-clock deadline, 1500 frames. M2 code; M3/M4 did not change the countdown loop.)*
- [ ] Same, with a laptop suspend/resume in the middle (verify the monotonic clock behaviour across suspend on Linux; if it pauses, compare against wall clock and fix). *(Fixed by design: wall-clock deadline; simulated in `TestCountdownEndsAfterSuspend`. Needs a real suspend/resume on a laptop.)*
- [x] `go test ./internal/timer` covers fraction and color math (100 % statement coverage).

## M3 — Notification and exit

### `internal/notify`
- [x] **T3.1** Add `github.com/godbus/dbus/v5`; connect to the session bus.
- [x] **T3.2** Call `org.freedesktop.Notifications.Notify` with app name `pomobar`, summary `Time's up`, body = label, sensible timeout.
- [x] **T3.3** On failure, log a one-line warning to stderr; never change the exit code.
- [x] **T3.4** Interface so `cmd/pomobar` can be tested with a fake notifier.

### End, cancel and errors
- [x] **T3.5** End of timer: send notification (unless `-quiet`), remove the bar, exit 0. (The bar is removed first, then the notification is sent; see `docs/decisions.md` D4.)
- [x] **T3.6** `signal.Notify` for SIGINT/SIGTERM: remove the bar immediately, no notification, exit 130 / 143.
- [x] **T3.7** Ensure teardown is flushed to the compositor before exit (roundtrip after destroy).
- [x] **T3.8** Missing layer-shell or failed Wayland connection: one-line error on stderr, exit 1.
- [x] **T3.9** Handle the compositor sending `closed` on the layer surface: exit cleanly (decide exit code, document it). Exit 1 with a one-line message, no notification (`docs/decisions.md` D5).
- [x] **T3.10** `-version` prints the injected version and exits 0.

### M3 acceptance

Automated coverage: `internal/notify` against a real `dbus-daemon` session
bus with a fake notification server; `cmd/pomobar` process tests send real
SIGINT/SIGTERM to the binary (exit 130/143, bar torn down, no notification)
and check the exit-1 paths against the fake compositor. The items below
still need manual verification on KDE Plasma (and GNOME for layer-shell).

- [ ] Notification appears in Plasma with the label as body. *(Verified on a test session bus; needs Plasma.)*
- [x] `-quiet` suppresses it. *(Unit test; Plasma check recommended with the rest.)*
- [ ] Ctrl-C removes the bar at once and sends no notification (exit 130); `kill` gives 143. *(Verified with real signals against the fake compositor and a real bus; needs an on-screen check on KDE.)*
- [ ] Running on a compositor without layer-shell (e.g. GNOME/Mutter) exits 1 with a readable error. *(Verified with the fake compositor hiding the global; needs a run on GNOME.)*
- [x] Running with no `$WAYLAND_DISPLAY` exits 1 with a readable error. *(Process test.)*

## M4 — Release

- [x] **T4.1** README: what it is, requirements (Wayland + wlr-layer-shell), install, usage, flags table, examples, exit codes, limitations (single monitor, no daemon).
- [ ] **T4.2** Record a demo GIF on KDE Plasma and embed it in the README.
- [ ] **T4.3** Verify `go install github.com/clobrano/pomobar/cmd/pomobar@latest` works and produces a static binary. *(Needs a tag on the default branch. Note: only static with `CGO_ENABLED=0`; with cgo available the toolchain links libc, so the README says `CGO_ENABLED=0 go install …`. `-version` reports the module version for `go install` builds.)*
- [x] **T4.4** Release automation (GoReleaser or a tag-triggered workflow): `CGO_ENABLED=0` binaries for linux/amd64 and linux/arm64, checksums, version injected. *(`.github/workflows/release.yml` on `v*` tags runs `make dist`: static check, tar.gz with README/LICENSE/notices, `SHA256SUMS`, `gh release create`. `make dist` verified locally; the workflow itself runs on the first tag.)*
- [x] **T4.5** Add a LICENSE (and third-party notice for the vendored wlr-protocols XML if applicable). *(MIT — confirm the choice. Nothing is vendored; `THIRD_PARTY_NOTICES.md` carries the licences of the statically linked Go runtime, go-wayland, godbus, x/sys and the Wayland/layer-shell protocol notices, and ships in the release archives.)*
- [ ] **T4.6** Tag `v0.1.0` and publish the GitHub release.

### M4 acceptance
- [ ] README with demo GIF is published.
- [ ] `go install` path works.
- [ ] GitHub release has linux/amd64 and linux/arm64 binaries.
- [ ] Full M1–M3 acceptance re-run on KDE Plasma Wayland with the release binary.

## M5 — Background timer and client commands

Requested after the MVP review: the timer should not block the terminal.

- [x] **T5.1** `pomobar <duration>` starts the timer in a detached process (re-exec with `-foreground`, `setsid`) and returns once the bar is shown; start errors are reported by the parent via a readiness pipe.
- [x] **T5.2** Control socket `$XDG_RUNTIME_DIR/pomobar.sock` with a one-line JSON protocol (`internal/ipc`); stale sockets are replaced.
- [x] **T5.3** Client commands `status`, `stop`, `pause`, `resume`; clear errors and exit 1 when no timer runs.
- [x] **T5.4** Refuse a second start while a timer runs, showing its status.
- [x] **T5.5** Pause/resume in `internal/timer` (frozen remaining time, new wall-clock deadline on resume); dimmed bar while paused.
- [x] **T5.6** Keep `-foreground` for scripts (exit 3 when stopped by a client).
- [x] **T5.7** Tests: ipc unit tests; process tests for detach, status, refusal, pause/resume (dimmed pixels), stop, completion with notification, foreground stopped by client, commands without a timer.

### M5 acceptance
- [ ] On KDE Plasma: `pomobar 25m` returns immediately, the bar stays after closing the terminal, and `status`/`pause`/`resume`/`stop` act on it.

## Open questions to resolve

- [x] Go Wayland library choice (T1.1, decide in M1): `vcaesar/go-wayland`, see `docs/decisions.md`.
- [ ] Should the bar flash/pulse at zero before disappearing?
- [ ] Should a second pomobar replace a running one, or coexist?
- [x] Binary/repo name: `pombar` vs `pomobar` → `pomobar`.

## Out of scope for the MVP (future work)

Monitor selection (`-output`) and per-monitor bars; a tmux segment
(`pomobar status` covers the basics); pomodoro cycles; config file;
end sound; fractional scaling via `wp_viewporter` + fractional-scale.
