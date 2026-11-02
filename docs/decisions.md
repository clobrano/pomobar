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
goroutine. pombar therefore:

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
