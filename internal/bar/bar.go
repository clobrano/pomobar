package bar

import (
	"cmp"
	"errors"
	"fmt"
	"image/color"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/vcaesar/go-wayland/client"
	wlr "github.com/vcaesar/go-wayland/wlr/layer-shell-v1"

	"github.com/clobrano/pomobar/internal/timer"
)

// Namespace is the layer-shell namespace of the bar surface.
const Namespace = "pomobar"

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

// Edge is the screen edge the bar is attached to.
type Edge int

// Screen edges.
const (
	Bottom Edge = iota
	Top
	Left
	Right
)

// String returns the edge name as used on the command line.
func (e Edge) String() string {
	switch e {
	case Bottom:
		return "bottom"
	case Top:
		return "top"
	case Left:
		return "left"
	case Right:
		return "right"
	}
	return fmt.Sprintf("Edge(%d)", int(e))
}

// ParseEdge parses "top", "bottom", "left" or "right".
func ParseEdge(s string) (Edge, error) {
	for _, e := range []Edge{Bottom, Top, Left, Right} {
		if s == e.String() {
			return e, nil
		}
	}
	return 0, fmt.Errorf("unknown edge %q (want top, bottom, left or right)", s)
}

// Vertical reports whether a bar on this edge runs vertically.
func (e Edge) Vertical() bool { return e == Left || e == Right }

// anchor is the layer-surface anchor: the edge plus its two neighbours, so
// the bar stretches along the whole edge.
func (e Edge) anchor() wlr.LayerSurfaceAnchor {
	const (
		top    = wlr.LayerSurfaceAnchorTop
		bottom = wlr.LayerSurfaceAnchorBottom
		left   = wlr.LayerSurfaceAnchorLeft
		right  = wlr.LayerSurfaceAnchorRight
	)
	switch e {
	case Top:
		return top | left | right
	case Left:
		return left | top | bottom
	case Right:
		return right | top | bottom
	default:
		return bottom | left | right
	}
}

// Options configures the bar.
type Options struct {
	// Edge the bar is attached to.
	Edge Edge
	// Thickness of the bar in surface-local (logical) pixels.
	Thickness int
	// Reserve makes the compositor keep the bar's strip free of other
	// windows (exclusive zone = thickness) instead of drawing over them.
	Reserve bool
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
	registry   *client.Registry
	compVer    uint32 // bound wl_compositor version
	globals    map[string]global
	outputs    map[uint32]*output // by global name
	entered    map[*client.Output]bool
	prefScale  int32 // wl_surface.preferred_buffer_scale, 0 if never sent
	scale      int32 // buffer scale in use

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
		globals:      map[string]global{},
		outputs:      map[uint32]*output{},
		entered:      map[*client.Output]bool{},
		scale:        1,
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

// output tracks a wl_output and its integer scale.
type output struct {
	wl    *client.Output
	scale int32
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
	b.registry = registry
	registry.SetGlobalHandler(b.onGlobal)
	registry.SetGlobalRemoveHandler(b.onGlobalRemove)
	if err := b.display.Roundtrip(); err != nil {
		return fmt.Errorf("%w: %w", ErrConnect, err)
	}
	if protoErr != nil {
		return protoErr
	}

	globals := b.globals
	bind := func(iface string, maxVersion uint32, p client.Proxy) (uint32, error) {
		g, ok := globals[iface]
		if !ok {
			return 0, fmt.Errorf("%w: %s", ErrMissingGlobal, iface)
		}
		v := min(g.version, maxVersion)
		return v, registry.Bind(g.name, iface, v, p)
	}
	if _, ok := globals[wlr.LayerShellInterfaceName]; !ok {
		return ErrNoLayerShell
	}
	b.compositor = client.NewCompositor(b.ctx)
	// v6 adds wl_surface.preferred_buffer_scale.
	if b.compVer, err = bind(client.CompositorInterfaceName, 6, b.compositor); err != nil {
		return err
	}
	b.shm = client.NewShm(b.ctx)
	if _, err := bind(client.ShmInterfaceName, 1, b.shm); err != nil {
		return err
	}
	b.layerShell = wlr.NewLayerShell(b.ctx)
	if _, err := bind(wlr.LayerShellInterfaceName, 4, b.layerShell); err != nil {
		return err
	}
	// Outputs were bound by onGlobal; collect their scale events.
	if err := b.display.Roundtrip(); err != nil {
		return fmt.Errorf("%w: %w", ErrConnect, err)
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
	b.surface.SetEnterHandler(b.onEnter)
	b.surface.SetLeaveHandler(b.onLeave)
	b.surface.SetPreferredBufferScaleHandler(b.onPreferredScale)
	if err := b.layer.SetAnchor(uint32(b.opts.Edge.anchor())); err != nil {
		return err
	}
	t := uint32(b.opts.Thickness)
	w, h := uint32(0), t
	if b.opts.Edge.Vertical() {
		w, h = t, 0
	}
	if err := b.layer.SetSize(w, h); err != nil {
		return err
	}
	zone := int32(0)
	if b.opts.Reserve {
		zone = int32(b.opts.Thickness)
	}
	if err := b.layer.SetExclusiveZone(zone); err != nil {
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

	b.scale = b.wantScale()
	if b.scale != 1 {
		if err := b.surface.SetBufferScale(b.scale); err != nil {
			return err
		}
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
	// The stretched dimension should never be 0; the thick one is 0 only
	// if the compositor leaves it to us.
	if b.opts.Edge.Vertical() {
		w, h = cmp.Or(w, b.opts.Thickness), cmp.Or(h, 1)
	} else {
		w, h = cmp.Or(w, 1), cmp.Or(h, b.opts.Thickness)
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
	bw, bh := b.width*int(b.scale), b.height*int(b.scale)
	vertical := b.opts.Edge.Vertical()
	length := bw
	if vertical {
		length = bh
	}
	filled := timer.FilledLength(b.fraction, length)
	if b.drawn && filled == b.drawnFilled && !timer.VisiblyDifferent(b.color, b.drawnColor) {
		return nil
	}

	var retired *shmPool
	var err error
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

	fill(buf.pix, bw, bh, b.pool.stride, vertical, filled, b.color)
	if err := b.surface.Attach(buf.wl, 0, 0); err != nil {
		return err
	}
	if b.compVer >= 4 {
		err = b.surface.DamageBuffer(0, 0, int32(bw), int32(bh))
	} else {
		err = b.surface.Damage(0, 0, int32(b.width), int32(b.height))
	}
	if err != nil {
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

// onGlobal records globals and binds every wl_output (also ones plugged
// in later) to learn its scale.
func (b *Bar) onGlobal(e client.RegistryGlobalEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	iface := strings.Clone(e.Interface)
	if iface == client.OutputInterfaceName {
		if b.closing {
			return
		}
		o := &output{wl: client.NewOutput(b.ctx), scale: 1}
		o.wl.SetScaleHandler(func(ev client.OutputScaleEvent) { b.onOutputScale(o, ev.Factor) })
		publish(o.wl)
		// v2 adds the scale event.
		if err := b.registry.Bind(e.Name, iface, min(e.Version, 2), o.wl); err != nil {
			return
		}
		b.outputs[e.Name] = o
		return
	}
	if _, ok := b.globals[iface]; !ok {
		b.globals[iface] = global{e.Name, e.Version}
	}
}

func (b *Bar) onGlobalRemove(e client.RegistryGlobalRemoveEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if o, ok := b.outputs[e.Name]; ok {
		delete(b.outputs, e.Name)
		delete(b.entered, o.wl)
		b.rescale()
	}
}

func (b *Bar) onOutputScale(o *output, factor int32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o.scale = max(factor, 1)
	b.rescale()
}

func (b *Bar) onEnter(e client.SurfaceEnterEvent) {
	if e.Output == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entered[e.Output] = true
	b.rescale()
}

func (b *Bar) onLeave(e client.SurfaceLeaveEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entered, e.Output)
	b.rescale()
}

func (b *Bar) onPreferredScale(e client.SurfacePreferredBufferScaleEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prefScale = max(e.Factor, 1)
	b.rescale()
}

// wantScale picks the integer buffer scale: the compositor's preferred
// scale if it sent one, else the largest scale of the outputs the surface
// is on, else the scale of the only output, else 1. b.mu must be held.
func (b *Bar) wantScale() int32 {
	if b.prefScale > 0 {
		return b.prefScale
	}
	var s int32
	for _, o := range b.outputs {
		if b.entered[o.wl] {
			s = max(s, o.scale)
		}
	}
	if s > 0 {
		return s
	}
	if len(b.outputs) == 1 {
		for _, o := range b.outputs {
			return o.scale
		}
	}
	return 1
}

// rescale switches to a new buffer scale and redraws. b.mu must be held.
func (b *Bar) rescale() {
	s := b.wantScale()
	if s == b.scale || b.surface == nil || b.closing {
		return
	}
	b.scale = s
	if err := b.surface.SetBufferScale(s); err != nil {
		return
	}
	b.drawn = false
	_ = b.render()
}
