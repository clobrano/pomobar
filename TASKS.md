# pombar — Task list

Derived from the pombar PRD (Oct 26, 2026). Tasks are grouped by milestone, in
the order they should be done. Each milestone ends with its acceptance
criteria as a checklist; the MVP is done when M1–M4 all pass on KDE Plasma
Wayland.

> Naming: the PRD calls the binary `pombar`, the repository is `pomobar`.
> Decide the final name (module path, binary, D-Bus app name, layer-shell
> namespace) before T0.1.

## M0 — Project bootstrap

- [ ] **T0.1** Initialise the Go module (`go mod init github.com/clobrano/pomobar`), pick a Go version, add `.gitignore` entries for build output.
- [ ] **T0.2** Create the package skeleton: `cmd/pombar`, `internal/timer`, `internal/bar`, `internal/notify`, `internal/protocol/layershell` (placeholder).
- [ ] **T0.3** Add a `Makefile` (or `justfile`) with `build` (`CGO_ENABLED=0`), `test`, `vet`, `lint` targets.
- [ ] **T0.4** CI workflow (GitHub Actions): `go vet`, `go test ./...`, and a `CGO_ENABLED=0` build for linux/amd64 and linux/arm64 on every push/PR.
- [ ] **T0.5** Inject version via `-ldflags "-X main.version=…"` so `-version` has something to print later.

## M1 — Layer-shell spike

Goal: a hard-coded binary that draws a 4 px green bar on the bottom edge for
60 s, shrinking, then exits.

### Library decision
- [ ] **T1.1** Evaluate `vcaesar/go-wayland` vs `xogas/wayland`: maintenance status, pure-Go (no cgo), scanner availability, `wl_shm`/`wl_region`/`wl_output` coverage, event-loop model. Record the decision and reasons in `docs/decisions.md` (resolves PRD open question 1).
- [ ] **T1.2** Check whether the chosen library ships `zwlr_layer_shell_v1` bindings.
- [ ] **T1.3** If not: vendor `wlr-layer-shell-unstable-v1.xml` from wlr-protocols (with its licence), generate bindings with the library's scanner into `internal/protocol/layershell`, and add a `go generate` directive so they can be regenerated.

### Wayland plumbing (`internal/bar`)
- [ ] **T1.4** Connect to `$WAYLAND_DISPLAY`; return a typed error if the variable is unset or the connection fails.
- [ ] **T1.5** Registry: bind `wl_compositor`, `wl_shm`, `zwlr_layer_shell_v1` (and `wl_output` for scale). Return a distinct "layer-shell not supported" error if the global is missing.
- [ ] **T1.6** Create `wl_surface`; call `get_layer_surface` with no output, layer `overlay`, namespace `pombar`.
- [ ] **T1.7** Anchor bottom + left + right, size `(0, 4)`, exclusive zone `0`, keyboard interactivity `none`.
- [ ] **T1.8** Set an empty `wl_region` as the input region (click-through).
- [ ] **T1.9** Initial commit → wait for `configure` → `ack_configure`; handle later `configure` events (size changes) and `closed`.
- [ ] **T1.10** shm buffer pool: `unix.MemfdCreate` + `ftruncate` + `mmap`, ARGB8888, two buffers; track `wl_buffer.release` so a busy buffer is never written.
- [ ] **T1.11** Draw function: fill the first `length × fraction` pixels with a color, the rest with transparent zero (premultiplied ARGB).
- [ ] **T1.12** Run the Wayland event loop on its own goroutine; expose a safe way for the main goroutine to request a redraw and to shut down.
- [ ] **T1.13** Clean teardown: destroy layer surface, surface, buffers, unmap memory, close the connection.

### Spike binary
- [ ] **T1.14** Temporary `cmd/pombar` main: fixed 60 s, green, bottom edge, redraw each second, exit 0.

### M1 acceptance (manual, on KDE Plasma Wayland)
- [ ] Bar is visible above normal windows and above fullscreen apps.
- [ ] Bar is visible on every virtual desktop.
- [ ] Clicks pass through the bar to the window beneath.
- [ ] `CGO_ENABLED=0 go build ./...` succeeds; `ldd` reports a static binary.
- [ ] If overlay misbehaves on KDE, switch to the `top` layer and note it in `docs/decisions.md` (PRD risk).

## M2 — CLI and timer

### `internal/timer` (pure, no Wayland)
- [ ] **T2.1** `Timer` type storing start and absolute deadline from `time.Now()` (monotonic); `Remaining(now)` and `Fraction(now)` clamped to `[0, 1]`; `Done(now)`.
- [ ] **T2.2** `Color(fraction)`: green `#4CAF50` above 50 %, amber `#FFB300` 50–20 %, red `#E53935` below 20 %, with smooth interpolation across each boundary. Define the exact blend width in a constant and document it.
- [ ] **T2.3** `FilledLength(fraction, length)` helper (integer pixels) used for "changed by ≥ 1 px" checks.
- [ ] **T2.4** Unit tests: fraction at start/middle/end/past deadline, zero/negative durations rejected, color at and around each threshold, interpolation monotonicity, filled-length rounding.

### CLI (`cmd/pombar`)
- [ ] **T2.5** Parse flags with the standard `flag` package: `-edge`, `-thickness`, `-reserve`, `-label`, `-quiet`, `-version`; positional `<duration>` via `time.ParseDuration`.
- [ ] **T2.6** Validate: missing/extra args, non-positive duration, unknown edge, thickness < 1. Print usage and exit 2 on invalid usage.
- [ ] **T2.7** Usage text matching the PRD examples.

### Bar options (`internal/bar`)
- [ ] **T2.8** Support all four edges: anchor edge + its two neighbours; size `(0, t)` for top/bottom, `(t, 0)` for left/right.
- [ ] **T2.9** Shrink direction: horizontal bars shrink toward the left, vertical bars toward the bottom (fill from the bottom up).
- [ ] **T2.10** `-reserve`: exclusive zone = thickness, otherwise 0.
- [ ] **T2.11** HiDPI: read integer scale from `wl_output`, call `wl_surface.set_buffer_scale`, allocate buffers at `size × scale`.
- [ ] **T2.12** Accept a color per frame from the caller (replace hard-coded green).

### Main loop
- [ ] **T2.13** 1 s ticker computes fraction and color from the monotonic deadline; only attach/commit when filled length changes by ≥ 1 px or the color changes visibly (define threshold).
- [ ] **T2.14** When remaining ≤ 0, stop ticking and proceed to the end-of-timer path.

### M2 acceptance
- [ ] All four edges render correctly and shrink in the documented direction.
- [ ] A 25 m timer ends within 1 s of wall-clock time.
- [ ] Same, with a laptop suspend/resume in the middle (verify the monotonic clock behaviour across suspend on Linux; if it pauses, compare against wall clock and fix).
- [ ] `go test ./internal/timer` covers fraction and color math.

## M3 — Notification and exit

### `internal/notify`
- [ ] **T3.1** Add `github.com/godbus/dbus/v5`; connect to the session bus.
- [ ] **T3.2** Call `org.freedesktop.Notifications.Notify` with app name `pombar`, summary `Time's up`, body = label, sensible timeout.
- [ ] **T3.3** On failure, log a one-line warning to stderr; never change the exit code.
- [ ] **T3.4** Interface so `cmd/pombar` can be tested with a fake notifier.

### End, cancel and errors
- [ ] **T3.5** End of timer: send notification (unless `-quiet`), remove the bar, exit 0.
- [ ] **T3.6** `signal.Notify` for SIGINT/SIGTERM: remove the bar immediately, no notification, exit 130 / 143.
- [ ] **T3.7** Ensure teardown is flushed to the compositor before exit (roundtrip after destroy).
- [ ] **T3.8** Missing layer-shell or failed Wayland connection: one-line error on stderr, exit 1.
- [ ] **T3.9** Handle the compositor sending `closed` on the layer surface: exit cleanly (decide exit code, document it).
- [ ] **T3.10** `-version` prints the injected version and exits 0.

### M3 acceptance
- [ ] Notification appears in Plasma with the label as body.
- [ ] `-quiet` suppresses it.
- [ ] Ctrl-C removes the bar at once and sends no notification (exit 130); `kill` gives 143.
- [ ] Running on a compositor without layer-shell (e.g. GNOME/Mutter) exits 1 with a readable error.
- [ ] Running with no `$WAYLAND_DISPLAY` exits 1 with a readable error.

## M4 — Release

- [ ] **T4.1** README: what it is, requirements (Wayland + wlr-layer-shell), install, usage, flags table, examples, exit codes, limitations (single monitor, no daemon).
- [ ] **T4.2** Record a demo GIF on KDE Plasma and embed it in the README.
- [ ] **T4.3** Verify `go install github.com/clobrano/pomobar/cmd/pombar@latest` works and produces a static binary.
- [ ] **T4.4** Release automation (GoReleaser or a tag-triggered workflow): `CGO_ENABLED=0` binaries for linux/amd64 and linux/arm64, checksums, version injected.
- [ ] **T4.5** Add a LICENSE (and third-party notice for the vendored wlr-protocols XML if applicable).
- [ ] **T4.6** Tag `v0.1.0` and publish the GitHub release.

### M4 acceptance
- [ ] README with demo GIF is published.
- [ ] `go install` path works.
- [ ] GitHub release has linux/amd64 and linux/arm64 binaries.
- [ ] Full M1–M3 acceptance re-run on KDE Plasma Wayland with the release binary.

## Open questions to resolve

- [ ] Go Wayland library choice (T1.1, decide in M1).
- [ ] Should the bar flash/pulse at zero before disappearing?
- [ ] Should a second pombar replace a running one, or coexist?
- [ ] Binary/repo name: `pombar` vs `pomobar`.

## Out of scope for the MVP (future work)

Monitor selection (`-output`) and per-monitor bars; daemon with
pause/resume/stop/status and a tmux segment; pomodoro cycles; config file;
end sound; fractional scaling via `wp_viewporter` + fractional-scale.
