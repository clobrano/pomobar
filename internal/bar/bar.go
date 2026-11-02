package bar

import (
	"errors"
	"fmt"
	"image/color"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/vcaesar/go-wayland/client"
	wlr "github.com/vcaesar/go-wayland/wlr/layer-shell-v1"
)

// Namespace is the layer-shell namespace of the bar surface.
const Namespace = "pombar"

// teardownTimeout bounds how long Close waits for the compositor to
// acknowledge that the bar surface is gone.
const teardownTimeout = 2 * time.Second

var (
	// ErrNoDisplay is returned when $WAYLAND_DISPLAY is not set.
	ErrNoDisplay = errors.New("WAYLAND_DISPLAY is not set: not running in a Wayland session")
	// ErrConnect wraps a failure to connect to the Wayland compositor.
	ErrConnect = errors.New("cannot connect to the Wayland compositor")
	// ErrNoLayerShell is returned when the compositor does not offer
	// zwlr_layer_shell_v1 (e.g. GNOME/Mutter).
	ErrNoLayerShell = errors.New("the compositor does not support wlr-layer-shell (zwlr_layer_shell_v1)")
	// ErrMissingGlobal is returned when a core global (wl_compositor,
	// wl_shm) is missing.
	ErrMissingGlobal = errors.New("the compositor is missing a required Wayland global")
	// ErrClosed is reported by Err when the compositor closed the bar
	// surface (for example because its output went away).
	ErrClosed = errors.New("the compositor closed the bar surface")
)

// Options configures the bar.
type Options struct {
	// Thickness of the bar in surface-local (logical) pixels.
	Thickness int
}

// Bar is a countdown bar drawn as a layer-shell surface. Its methods are
// safe for concurrent use.
type Bar struct {
	opts Options

	ctx     *client.Context
	display *client.Display

	// mu guards everything below and serializes every request we send, so
	// object-creating requests reach the compositor in id order.
	mu         sync.Mutex
	compositor *client.Compositor
	shm        *client.Shm
	layerShell *wlr.LayerShell
	surface    *client.Surface
	layer      *wlr.LayerSurface

	configured    bool
	width, height int // surface size from the last configure, logical px

	pool    *shmPool
	pending bool // a frame is waiting for a free buffer

	// requested state
	requested bool // Draw has been called at least once
	fraction  float64
	color     color.RGBA

	// last committed state
	drawn       bool
	drawnFilled int
	drawnColor  color.RGBA

	closing  bool
	err      error
	done     chan struct{} // closed when the bar can no longer be shown
	doneOnce sync.Once

	dispatchDone chan struct{}
}

// Open connects to the compositor named by $WAYLAND_DISPLAY and maps an
// (initially empty) bar.
func Open(opts Options) (*Bar, error) {
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil, ErrNoDisplay
	}
	return OpenAddr("", opts)
}

// OpenAddr is like Open but connects to the given socket path. An empty
// addr uses $WAYLAND_DISPLAY and $XDG_RUNTIME_DIR.
func OpenAddr(addr string, opts Options) (*Bar, error) {
	if opts.Thickness < 1 {
		return nil, fmt.Errorf("invalid thickness %d", opts.Thickness)
	}
	display, err := client.Connect(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConnect, err)
	}
	b := &Bar{
		opts:         opts,
		ctx:          display.Context(),
		display:      display,
		done:         make(chan struct{}),
		dispatchDone: make(chan struct{}),
	}
	if err := b.setup(); err != nil {
		_ = b.ctx.Close()
		return nil, err
	}
	go b.dispatchLoop()
	return b, nil
}

type global struct {
	name    uint32
	version uint32
}

// setup runs on the caller's goroutine before the dispatch goroutine
// starts, so it may use Roundtrip.
func (b *Bar) setup() error {
	var protoErr error
	b.display.SetErrorHandler(func(e client.DisplayErrorEvent) {
		b.fail(protocolError(e))
		protoErr = b.err
	})

	registry, err := b.display.GetRegistry()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrConnect, err)
	}
	globals := map[string]global{}
	registry.SetGlobalHandler(func(e client.RegistryGlobalEvent) {
		if _, ok := globals[e.Interface]; !ok {
			globals[strings.Clone(e.Interface)] = global{e.Name, e.Version}
		}
	})
	if err := b.display.Roundtrip(); err != nil {
		return fmt.Errorf("%w: %w", ErrConnect, err)
	}
	if protoErr != nil {
		return protoErr
	}

	bind := func(iface string, maxVersion uint32, p client.Proxy) error {
		g, ok := globals[iface]
		if !ok {
			return fmt.Errorf("%w: %s", ErrMissingGlobal, iface)
		}
		return registry.Bind(g.name, iface, min(g.version, maxVersion), p)
	}
	if _, ok := globals[wlr.LayerShellInterfaceName]; !ok {
		return ErrNoLayerShell
	}
	b.compositor = client.NewCompositor(b.ctx)
	if err := bind(client.CompositorInterfaceName, 4, b.compositor); err != nil {
		return err
	}
	b.shm = client.NewShm(b.ctx)
	if err := bind(client.ShmInterfaceName, 1, b.shm); err != nil {
		return err
	}
	b.layerShell = wlr.NewLayerShell(b.ctx)
	if err := bind(wlr.LayerShellInterfaceName, 4, b.layerShell); err != nil {
		return err
	}

	if b.surface, err = b.compositor.CreateSurface(); err != nil {
		return err
	}
	// No output: let the compositor pick (normally the focused one).
	b.layer, err = b.layerShell.GetLayerSurface(b.surface, nil, uint32(wlr.LayerShellLayerOverlay), Namespace)
	if err != nil {
		return err
	}
	b.layer.SetConfigureHandler(b.onConfigure)
	b.layer.SetClosedHandler(func(wlr.LayerSurfaceClosedEvent) { b.fail(ErrClosed) })
	anchor := wlr.LayerSurfaceAnchorBottom | wlr.LayerSurfaceAnchorLeft | wlr.LayerSurfaceAnchorRight
	if err := b.layer.SetAnchor(uint32(anchor)); err != nil {
		return err
	}
	if err := b.layer.SetSize(0, uint32(b.opts.Thickness)); err != nil {
		return err
	}
	if err := b.layer.SetExclusiveZone(0); err != nil {
		return err
	}
	if err := b.layer.SetKeyboardInteractivity(uint32(wlr.LayerSurfaceKeyboardInteractivityNone)); err != nil {
		return err
	}

	// Empty input region: pointer and touch input pass through the bar.
	region, err := b.compositor.CreateRegion()
	if err != nil {
		return err
	}
	if err := b.surface.SetInputRegion(region); err != nil {
		return err
	}
	if err := region.Destroy(); err != nil {
		return err
	}

	// Initial commit without a buffer; the compositor answers with
	// configure, which the dispatch goroutine handles.
	if err := b.surface.Commit(); err != nil {
		return err
	}
	if err := b.display.Roundtrip(); err != nil {
		return fmt.Errorf("%w: %w", ErrConnect, err)
	}
	return protoErr
}

func protocolError(e client.DisplayErrorEvent) error {
	obj := uint32(0)
	if e.ObjectId != nil {
		obj = e.ObjectId.ID()
	}
	return fmt.Errorf("wayland protocol error on object %d (code %d): %s", obj, e.Code, strings.Clone(e.Message))
}

func (b *Bar) dispatchLoop() {
	defer close(b.dispatchDone)
	for {
		err := b.ctx.Dispatch()
		if err == nil {
			continue
		}
		if !errors.Is(err, client.ErrDispatchUnableToReadMsg) {
			// An event for an object we do not know about: ignore it.
			continue
		}
		b.mu.Lock()
		closing := b.closing
		b.mu.Unlock()
		if !closing {
			b.fail(fmt.Errorf("lost connection to the compositor: %w", err))
		}
		return
	}
}

// fail records the first fatal error and signals Done.
func (b *Bar) fail(err error) {
	b.mu.Lock()
	if b.err == nil {
		b.err = err
	}
	b.mu.Unlock()
	b.doneOnce.Do(func() { close(b.done) })
}

// Done is closed when the bar can no longer be shown: the compositor
// closed the surface, sent a protocol error or the connection was lost.
// Err reports why.
func (b *Bar) Done() <-chan struct{} { return b.done }

// Err returns the reason Done was closed, or nil.
func (b *Bar) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func (b *Bar) onConfigure(e wlr.LayerSurfaceConfigureEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing {
		return
	}
	if err := b.layer.AckConfigure(e.Serial); err != nil {
		return
	}
	w, h := int(e.Width), int(e.Height)
	if h == 0 {
		h = b.opts.Thickness
	}
	if w == 0 {
		w = 1 // anchored left+right, the compositor should never send 0
	}
	if !b.configured || w != b.width || h != b.height {
		b.drawn = false // size changed: force a full redraw
	}
	b.configured = true
	b.width, b.height = w, h
	b.render()
}

// Draw sets the filled fraction (0–1) and color of the bar and commits a
// new frame if anything visibly changed.
func (b *Bar) Draw(fraction float64, c color.RGBA) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing || b.err != nil {
		return b.err
	}
	b.requested, b.fraction, b.color = true, fraction, c
	return b.render()
}

// render draws the requested state into a free buffer and commits it.
// b.mu must be held.
func (b *Bar) render() error {
	if !b.configured || !b.requested || b.closing {
		return nil
	}
	bw, bh := b.width, b.height
	length := bw
	filled := filledPixels(b.fraction, length)
	if b.drawn && filled == b.drawnFilled && b.color == b.drawnColor {
		return nil
	}

	var retired *shmPool
	if b.pool == nil || b.pool.width != bw || b.pool.height != bh {
		p, err := newShmPool(b.shm, bw, bh, b.onRelease)
		if err != nil {
			return err
		}
		retired, b.pool = b.pool, p
	}
	buf := b.pool.free()
	if buf == nil {
		b.pending = true
		return nil
	}
	b.pending = false

	fill(buf.pix, bw, bh, b.pool.stride, false, filled, b.color)
	if err := b.surface.Attach(buf.wl, 0, 0); err != nil {
		return err
	}
	if err := b.surface.Damage(0, 0, int32(b.width), int32(b.height)); err != nil {
		return err
	}
	if err := b.surface.Commit(); err != nil {
		return err
	}
	buf.busy = true
	b.drawn, b.drawnFilled, b.drawnColor = true, filled, b.color
	if retired != nil {
		retired.destroy()
	}
	return nil
}

func (b *Bar) onRelease(buf *shmBuffer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	buf.busy = false
	if b.pending {
		_ = b.render()
	}
}

// Close removes the bar and closes the connection. It waits (bounded by
// teardownTimeout) until the compositor has processed the destroy
// requests, so the bar is gone from the screen when Close returns.
func (b *Bar) Close() error {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		<-b.dispatchDone
		return nil
	}
	b.closing = true
	if b.layer != nil {
		_ = b.layer.Destroy()
	}
	if b.surface != nil {
		_ = b.surface.Destroy()
	}
	if b.pool != nil {
		b.pool.destroy()
		b.pool = nil
	}
	synced := make(chan struct{})
	syncErr := b.sync(func() { close(synced) })
	b.mu.Unlock()

	if syncErr == nil {
		select {
		case <-synced:
		case <-b.dispatchDone:
		case <-time.After(teardownTimeout):
		}
	}
	err := b.ctx.Close()
	<-b.dispatchDone
	return err
}

// sync sends wl_display.sync and calls done from the dispatch goroutine
// when the compositor answers. Unlike client.Display.Sync it installs the
// handler before the request is sent, so the answer cannot be missed.
// b.mu must be held.
func (b *Bar) sync(done func()) error {
	cb := client.NewCallback(b.ctx)
	cb.SetDoneHandler(func(client.CallbackDoneEvent) {
		b.ctx.Unregister(cb)
		done()
	})
	publish(cb)
	const opcode = 0 // wl_display.sync
	var req [12]byte
	client.PutUint32(req[0:4], b.display.ID())
	client.PutUint32(req[4:8], uint32(len(req))<<16|opcode)
	client.PutUint32(req[8:12], cb.ID())
	return b.ctx.WriteMsg(req[:], nil)
}
