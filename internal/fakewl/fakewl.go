// Package fakewl is a minimal in-process Wayland compositor used to test
// pomobar without a real compositor. It speaks the wire protocol over a unix
// socket and implements just enough of wl_display, wl_registry,
// wl_compositor, wl_surface, wl_region, wl_shm, wl_shm_pool, wl_buffer,
// wl_output and zwlr_layer_shell_v1 to map a layer surface, receive shm
// buffers and inspect what the client drew.
//
// It also enforces the layer-shell rules a real compositor would: no buffer
// before the first ack_configure, and a protocol error otherwise.
package fakewl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Options configures the fake compositor.
type Options struct {
	// NoLayerShell hides zwlr_layer_shell_v1, like GNOME/Mutter.
	NoLayerShell bool
	// OutputWidth and OutputHeight are the logical output size used to
	// answer configure for anchored dimensions (defaults 1920×1080).
	OutputWidth, OutputHeight int
	// OutputScale is sent as wl_output.scale (default 1).
	OutputScale int32
	// PreferredScale, if non-zero, is sent as
	// wl_surface.preferred_buffer_scale (wl_compositor v6).
	PreferredScale int32
	// CompositorVersion advertised for wl_compositor (default 6).
	CompositorVersion uint32
	// NoDeleteID suppresses wl_display.delete_id (to test id reuse paths).
	NoDeleteID bool
}

// Frame is one committed buffer, copied out of the client's shm pool.
type Frame struct {
	Width, Height int
	Stride        int
	Format        uint32
	Pix           []byte
	BufferScale   int32
}

// Pixel returns the ARGB8888 value at (x, y).
func (f Frame) Pixel(x, y int) uint32 {
	return binary.LittleEndian.Uint32(f.Pix[y*f.Stride+x*4:])
}

// LayerSurface is the state of the client's layer surface.
type LayerSurface struct {
	Created               bool
	Namespace             string
	Layer                 uint32
	OutputID              uint32
	Anchor                uint32
	Width, Height         uint32 // requested via set_size
	ExclusiveZone         int32
	KeyboardInteractivity uint32
	Margin                [4]int32
	Acked                 bool
	ConfigureSerial       uint32
	ConfiguredW           uint32
	ConfiguredH           uint32
	Destroyed             bool
}

// State is a snapshot of what the client has done.
type State struct {
	Connected      bool
	Disconnected   bool
	Layer          LayerSurface
	InputRegionSet bool
	InputRegion    []Rect // rectangles added to the input region
	SurfaceAlive   bool
	BufferScale    int32
	Commits        int
	Frames         []Frame
	LiveBuffers    int
	ProtocolError  string
	SyncsAfterGone int // wl_display.sync received after the layer surface was destroyed

	layerID uint32 // object id of the layer surface, 0 if none
	serial  uint32 // last configure serial sent
}

// Rect is a region rectangle.
type Rect struct{ X, Y, W, H int32 }

// LastFrame returns the most recent frame, if any.
func (s State) LastFrame() (Frame, bool) {
	if len(s.Frames) == 0 {
		return Frame{}, false
	}
	return s.Frames[len(s.Frames)-1], true
}

// Server is a running fake compositor.
type Server struct {
	opts Options
	dir  string
	path string
	ln   *net.UnixListener

	mu      sync.Mutex
	cond    *sync.Cond
	state   State
	conn    *conn
	wg      sync.WaitGroup
	closing bool
}

// Start listens on a fresh socket in a temporary directory.
func Start(opts Options) (*Server, error) {
	if opts.OutputWidth == 0 {
		opts.OutputWidth = 1920
	}
	if opts.OutputHeight == 0 {
		opts.OutputHeight = 1080
	}
	if opts.OutputScale == 0 {
		opts.OutputScale = 1
	}
	if opts.CompositorVersion == 0 {
		opts.CompositorVersion = 6
	}
	dir, err := os.MkdirTemp("", "fakewl")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "wayland-fake")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	s := &Server{opts: opts, dir: dir, path: path, ln: ln}
	s.cond = sync.NewCond(&s.mu)
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Dir is the directory holding the socket (use as XDG_RUNTIME_DIR).
func (s *Server) Dir() string { return s.dir }

// Path is the socket path.
func (s *Server) Path() string { return s.path }

// Name is the socket name relative to Dir (use as WAYLAND_DISPLAY).
func (s *Server) Name() string { return filepath.Base(s.path) }

// Close stops the server and removes its socket.
func (s *Server) Close() {
	s.mu.Lock()
	s.closing = true
	c := s.conn
	s.mu.Unlock()
	s.ln.Close()
	if c != nil {
		c.c.Close()
	}
	s.wg.Wait()
	os.RemoveAll(s.dir)
}

// Snapshot returns a copy of the current state.
func (s *Server) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	st.Frames = slices.Clone(st.Frames)
	st.InputRegion = slices.Clone(st.InputRegion)
	return st
}

// WaitFor blocks until cond holds for the state or the timeout expires.
func (s *Server) WaitFor(timeout time.Duration, cond func(State) bool) (State, error) {
	deadline := time.Now().Add(timeout)
	t := time.AfterFunc(timeout, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer t.Stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		st := s.state
		st.Frames = slices.Clone(st.Frames)
		if cond(st) {
			return st, nil
		}
		if !time.Now().Before(deadline) {
			return st, errors.New("fakewl: timed out waiting for condition")
		}
		s.cond.Wait()
	}
}

// SendClosed sends zwlr_layer_surface_v1.closed to the client.
func (s *Server) SendClosed() error {
	s.mu.Lock()
	c := s.conn
	ls := s.state.layerID
	s.mu.Unlock()
	if c == nil || ls == 0 {
		return errors.New("fakewl: no layer surface")
	}
	return c.send(ls, 1)
}

// Reconfigure sends a new configure event with the given size.
func (s *Server) Reconfigure(w, h uint32) error {
	s.mu.Lock()
	c := s.conn
	ls := s.state.layerID
	s.state.serial++
	serial := s.state.serial
	s.state.Layer.ConfigureSerial = serial
	s.state.Layer.ConfiguredW, s.state.Layer.ConfiguredH = w, h
	s.mu.Unlock()
	if c == nil || ls == 0 {
		return errors.New("fakewl: no layer surface")
	}
	return c.send(ls, 0, serial, w, h)
}

func (s *Server) update(f func(*State)) {
	s.mu.Lock()
	f(&s.state)
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		uc, err := s.ln.AcceptUnix()
		if err != nil {
			return
		}
		c := newConn(s, uc)
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			uc.Close()
			return
		}
		s.conn = c
		s.state = State{Connected: true}
		s.cond.Broadcast()
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			c.serve()
		}()
	}
}

// ---- connection ----

type object struct {
	iface   string
	version uint32
	data    any
}

type pool struct {
	mem []byte
}

type buffer struct {
	pool                 *pool
	offset, w, h, stride int32
	format               uint32
	destroyed            bool
}

type surface struct {
	pendingBuffer   uint32
	pendingAttached bool
	current         uint32
	scale           int32
	pendingScale    int32
	committedOnce   bool
	entered         bool
}

type conn struct {
	s  *Server
	c  *net.UnixConn
	wm sync.Mutex

	objects      map[uint32]*object
	fds          []int
	layerSurface uint32
	surfaceID    uint32
	outputID     uint32
	configured   bool
}

const (
	globalCompositor = 1
	globalShm        = 2
	globalLayerShell = 3
	globalOutput     = 4
)

func newConn(s *Server, c *net.UnixConn) *conn {
	return &conn{s: s, c: c, objects: map[uint32]*object{1: {iface: "wl_display", version: 1}}}
}

// send writes an event. Arguments may be uint32, int32 or string.
func (c *conn) send(id uint32, opcode uint16, args ...any) error {
	var body []byte
	for _, a := range args {
		switch v := a.(type) {
		case uint32:
			body = binary.LittleEndian.AppendUint32(body, v)
		case int32:
			body = binary.LittleEndian.AppendUint32(body, uint32(v))
		case string:
			n := len(v) + 1
			body = binary.LittleEndian.AppendUint32(body, uint32(n))
			body = append(body, v...)
			body = append(body, 0)
			for len(body)%4 != 0 {
				body = append(body, 0)
			}
		default:
			panic(fmt.Sprintf("fakewl: bad arg %T", a))
		}
	}
	msg := binary.LittleEndian.AppendUint32(nil, id)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(8+len(body))<<16|uint32(opcode))
	msg = append(msg, body...)
	c.wm.Lock()
	defer c.wm.Unlock()
	_, err := c.c.Write(msg)
	return err
}

func (c *conn) protocolError(obj, code uint32, msg string) {
	c.s.update(func(st *State) {
		if st.ProtocolError == "" {
			st.ProtocolError = msg
		}
	})
	_ = c.send(1, 0, obj, code, msg)
	c.c.Close()
}

func (c *conn) deleteID(id uint32) {
	delete(c.objects, id)
	if !c.s.opts.NoDeleteID {
		_ = c.send(1, 1, id)
	}
}

func (c *conn) serve() {
	defer func() {
		c.c.Close()
		for _, fd := range c.fds {
			unix.Close(fd)
		}
		for _, o := range c.objects {
			if p, ok := o.data.(*pool); ok && p.mem != nil {
				unix.Munmap(p.mem)
			}
		}
		c.s.update(func(st *State) { st.Disconnected = true })
	}()
	var buf []byte
	rb := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(28*4))
	for {
		n, oobn, _, _, err := c.c.ReadMsgUnix(rb, oob)
		if oobn > 0 {
			if msgs, perr := unix.ParseSocketControlMessage(oob[:oobn]); perr == nil {
				for _, m := range msgs {
					if fds, ferr := unix.ParseUnixRights(&m); ferr == nil {
						c.fds = append(c.fds, fds...)
					}
				}
			}
		}
		if n > 0 {
			buf = append(buf, rb[:n]...)
		}
		for len(buf) >= 8 {
			size := int(binary.LittleEndian.Uint32(buf[4:]) >> 16)
			if size < 8 || len(buf) < size {
				break
			}
			id := binary.LittleEndian.Uint32(buf)
			op := uint16(binary.LittleEndian.Uint32(buf[4:]) & 0xffff)
			body := slices.Clone(buf[8:size])
			buf = buf[size:]
			if !c.handle(id, op, &reader{b: body}) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

type reader struct {
	b   []byte
	err bool
}

func (r *reader) u32() uint32 {
	if len(r.b) < 4 {
		r.err = true
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) i32() int32 { return int32(r.u32()) }

func (r *reader) str() string {
	n := int(r.u32())
	p := (n + 3) &^ 3
	if len(r.b) < p || n == 0 {
		r.err = true
		return ""
	}
	s := string(r.b[:n-1])
	r.b = r.b[p:]
	return s
}

func (c *conn) popFd() int {
	if len(c.fds) == 0 {
		return -1
	}
	fd := c.fds[0]
	c.fds = c.fds[1:]
	return fd
}

func (c *conn) newObject(id uint32, iface string, version uint32, data any) bool {
	if _, ok := c.objects[id]; ok || id == 0 {
		c.protocolError(1, 0, fmt.Sprintf("invalid new id %d for %s", id, iface))
		return false
	}
	c.objects[id] = &object{iface: iface, version: version, data: data}
	return true
}

// handle processes one request; it returns false when the connection must
// be dropped.
func (c *conn) handle(id uint32, op uint16, r *reader) bool {
	o, ok := c.objects[id]
	if !ok {
		c.protocolError(1, 0, fmt.Sprintf("request on unknown object %d", id))
		return false
	}
	switch o.iface {
	case "wl_display":
		switch op {
		case 0: // sync
			cb := r.u32()
			c.s.update(func(st *State) {
				if st.Layer.Destroyed {
					st.SyncsAfterGone++
				}
			})
			c.objects[cb] = &object{iface: "wl_callback", version: 1}
			_ = c.send(cb, 0, uint32(0))
			c.deleteID(cb)
		case 1: // get_registry
			reg := r.u32()
			if !c.newObject(reg, "wl_registry", 1, nil) {
				return false
			}
			_ = c.send(reg, 0, uint32(globalCompositor), "wl_compositor", c.s.opts.CompositorVersion)
			_ = c.send(reg, 0, uint32(globalShm), "wl_shm", uint32(1))
			if !c.s.opts.NoLayerShell {
				_ = c.send(reg, 0, uint32(globalLayerShell), "zwlr_layer_shell_v1", uint32(4))
			}
			_ = c.send(reg, 0, uint32(globalOutput), "wl_output", uint32(4))
		}
	case "wl_registry":
		if op != 0 {
			break
		}
		name := r.u32()
		iface := r.str()
		version := r.u32()
		nid := r.u32()
		if r.err {
			c.protocolError(id, 0, "bad bind")
			return false
		}
		want := map[uint32]string{globalCompositor: "wl_compositor", globalShm: "wl_shm", globalLayerShell: "zwlr_layer_shell_v1", globalOutput: "wl_output"}
		if want[name] != iface || (name == globalLayerShell && c.s.opts.NoLayerShell) {
			c.protocolError(id, 0, fmt.Sprintf("bind: bad global %d %q", name, iface))
			return false
		}
		if !c.newObject(nid, iface, version, nil) {
			return false
		}
		switch iface {
		case "wl_shm":
			_ = c.send(nid, 0, uint32(0)) // argb8888
			_ = c.send(nid, 0, uint32(1)) // xrgb8888
		case "wl_output":
			c.outputID = nid
			if version >= 2 {
				_ = c.send(nid, 3, c.s.opts.OutputScale)
				_ = c.send(nid, 2)
			}
		}
	case "wl_compositor":
		nid := r.u32()
		switch op {
		case 0:
			if !c.newObject(nid, "wl_surface", o.version, &surface{scale: 1, pendingScale: 1}) {
				return false
			}
			c.surfaceID = nid
			c.s.update(func(st *State) { st.SurfaceAlive = true; st.BufferScale = 1 })
		case 1:
			if !c.newObject(nid, "wl_region", 1, &[]Rect{}) {
				return false
			}
		}
	case "wl_region":
		rects := o.data.(*[]Rect)
		switch op {
		case 0:
			c.deleteID(id)
		case 1:
			*rects = append(*rects, Rect{r.i32(), r.i32(), r.i32(), r.i32()})
		}
	case "wl_shm":
		if op == 0 {
			nid := r.u32()
			size := r.i32()
			fd := c.popFd()
			if fd < 0 {
				c.protocolError(id, 0, "create_pool without fd")
				return false
			}
			mem, err := unix.Mmap(fd, 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
			unix.Close(fd)
			if err != nil {
				c.protocolError(id, 2, "mmap failed: "+err.Error())
				return false
			}
			if !c.newObject(nid, "wl_shm_pool", 1, &pool{mem: mem}) {
				return false
			}
		}
	case "wl_shm_pool":
		p := o.data.(*pool)
		switch op {
		case 0:
			nid := r.u32()
			b := &buffer{pool: p, offset: r.i32(), w: r.i32(), h: r.i32(), stride: r.i32(), format: r.u32()}
			if int(b.offset)+int(b.stride)*int(b.h) > len(p.mem) || b.stride < b.w*4 {
				c.protocolError(id, 2, "buffer outside pool")
				return false
			}
			if !c.newObject(nid, "wl_buffer", 1, b) {
				return false
			}
			c.s.update(func(st *State) { st.LiveBuffers++ })
		case 1:
			// The pool memory stays mapped while buffers reference it.
			c.deleteID(id)
		}
	case "wl_buffer":
		if op == 0 {
			o.data.(*buffer).destroyed = true
			c.deleteID(id)
			c.s.update(func(st *State) { st.LiveBuffers-- })
		}
	case "wl_surface":
		return c.handleSurface(id, o, op, r)
	case "wl_output":
		if op == 0 {
			c.deleteID(id)
		}
	case "zwlr_layer_shell_v1":
		switch op {
		case 0:
			nid := r.u32()
			surf := r.u32()
			out := r.u32()
			layer := r.u32()
			ns := r.str()
			if surf != c.surfaceID {
				c.protocolError(id, 0, "get_layer_surface: unknown surface")
				return false
			}
			if !c.newObject(nid, "zwlr_layer_surface_v1", o.version, nil) {
				return false
			}
			c.layerSurface = nid
			c.s.update(func(st *State) {
				st.Layer = LayerSurface{Created: true, Namespace: ns, Layer: layer, OutputID: out}
				st.layerID = nid
			})
		case 1:
			c.deleteID(id)
		}
	case "zwlr_layer_surface_v1":
		return c.handleLayerSurface(id, op, r)
	case "wl_callback":
	default:
		c.protocolError(id, 0, "unsupported interface "+o.iface)
		return false
	}
	return true
}

func (c *conn) handleSurface(id uint32, o *object, op uint16, r *reader) bool {
	s := o.data.(*surface)
	switch op {
	case 0: // destroy
		c.deleteID(id)
		c.s.update(func(st *State) { st.SurfaceAlive = false })
	case 1: // attach
		buf := r.u32()
		x, y := r.i32(), r.i32()
		if o.version >= 5 && (x != 0 || y != 0) {
			c.protocolError(id, 3, "attach with non-zero offset")
			return false
		}
		if c.layerSurface != 0 && !c.configured {
			c.protocolError(c.layerSurface, 0, "attach before first ack_configure")
			return false
		}
		s.pendingBuffer, s.pendingAttached = buf, true
	case 2, 9: // damage, damage_buffer
	case 5: // set_input_region
		rid := r.u32()
		var rects []Rect
		if ro, ok := c.objects[rid]; ok {
			rects = slices.Clone(*ro.data.(*[]Rect))
		}
		c.s.update(func(st *State) { st.InputRegionSet = true; st.InputRegion = rects })
	case 8: // set_buffer_scale
		s.pendingScale = r.i32()
	case 6: // commit
		s.scale = s.pendingScale
		var frame *Frame
		if s.pendingAttached {
			prev := s.current
			s.current = s.pendingBuffer
			s.pendingAttached = false
			if bo, ok := c.objects[s.current]; ok && s.current != 0 {
				b := bo.data.(*buffer)
				if b.w%s.scale != 0 || b.h%s.scale != 0 {
					c.protocolError(id, 2, "buffer size not a multiple of scale")
					return false
				}
				f := Frame{Width: int(b.w), Height: int(b.h), Stride: int(b.stride), Format: b.format, BufferScale: s.scale}
				f.Pix = slices.Clone(b.pool.mem[b.offset : b.offset+b.stride*b.h])
				frame = &f
			}
			if prev != 0 && prev != s.current {
				if _, ok := c.objects[prev]; ok {
					_ = c.send(prev, 0) // wl_buffer.release
				}
			}
		}
		c.s.update(func(st *State) {
			st.Commits++
			st.BufferScale = s.scale
			if frame != nil {
				st.Frames = append(st.Frames, *frame)
			}
		})
		if c.layerSurface != 0 && !s.committedOnce {
			s.committedOnce = true
			c.sendConfigure()
		}
		if frame != nil && !s.entered && c.outputID != 0 {
			s.entered = true
			_ = c.send(id, 0, c.outputID) // enter
			if c.s.opts.PreferredScale != 0 && o.version >= 6 {
				_ = c.send(id, 2, c.s.opts.PreferredScale)
			}
		}
	}
	return true
}

func (c *conn) sendConfigure() {
	st := c.s.Snapshot().Layer
	w, h := st.Width, st.Height
	const left, right, top, bottom = 4, 8, 1, 2
	if st.Anchor&(left|right) == left|right && w == 0 {
		w = uint32(c.s.opts.OutputWidth)
	}
	if st.Anchor&(top|bottom) == top|bottom && h == 0 {
		h = uint32(c.s.opts.OutputHeight)
	}
	var serial uint32
	c.s.update(func(st *State) {
		st.serial++
		serial = st.serial
		st.Layer.ConfigureSerial = serial
		st.Layer.ConfiguredW, st.Layer.ConfiguredH = w, h
	})
	_ = c.send(c.layerSurface, 0, serial, w, h)
}

func (c *conn) handleLayerSurface(id uint32, op uint16, r *reader) bool {
	var f func(*LayerSurface)
	switch op {
	case 0:
		w, h := r.u32(), r.u32()
		f = func(l *LayerSurface) { l.Width, l.Height = w, h }
	case 1:
		a := r.u32()
		f = func(l *LayerSurface) { l.Anchor = a }
	case 2:
		z := r.i32()
		f = func(l *LayerSurface) { l.ExclusiveZone = z }
	case 3:
		m := [4]int32{r.i32(), r.i32(), r.i32(), r.i32()}
		f = func(l *LayerSurface) { l.Margin = m }
	case 4:
		k := r.u32()
		f = func(l *LayerSurface) { l.KeyboardInteractivity = k }
	case 6:
		serial := r.u32()
		if serial == 0 || serial > c.s.Snapshot().serial {
			c.protocolError(id, 0, "ack_configure with invalid serial")
			return false
		}
		c.configured = true
		f = func(l *LayerSurface) { l.Acked = true }
	case 7:
		c.deleteID(id)
		c.layerSurface = 0
		c.s.update(func(st *State) { st.layerID = 0 })
		f = func(l *LayerSurface) { l.Destroyed = true }
	case 8:
		layer := r.u32()
		f = func(l *LayerSurface) { l.Layer = layer }
	default:
		return true
	}
	if r.err {
		c.protocolError(id, 0, "short request")
		return false
	}
	c.s.update(func(st *State) { f(&st.Layer) })
	return true
}
