package bar

import (
	"errors"
	"image/color"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/clobrano/pomobar/internal/fakewl"
)

var green = color.RGBA{0x4C, 0xAF, 0x50, 0xFF}

const wait = 5 * time.Second

func startServer(t *testing.T, opts fakewl.Options) *fakewl.Server {
	t.Helper()
	s, err := fakewl.Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func openBar(t *testing.T, s *fakewl.Server, opts Options) *Bar {
	t.Helper()
	b, err := OpenAddr(s.Path(), opts)
	if err != nil {
		t.Fatalf("OpenAddr: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func lastFrame(t *testing.T, s *fakewl.Server, cond func(fakewl.Frame) bool) fakewl.Frame {
	t.Helper()
	st, err := s.WaitFor(wait, func(st fakewl.State) bool {
		f, ok := st.LastFrame()
		return ok && cond(f)
	})
	if err != nil {
		f, _ := st.LastFrame()
		t.Fatalf("%v (frames=%d last=%dx%d)", err, len(st.Frames), f.Width, f.Height)
	}
	f, _ := st.LastFrame()
	return f
}

func TestNoDisplay(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "")
	_, err := Open(Options{Thickness: 4})
	if !errors.Is(err, ErrNoDisplay) {
		t.Fatalf("got %v, want ErrNoDisplay", err)
	}
}

func TestConnectFailure(t *testing.T) {
	_, err := OpenAddr(filepath.Join(t.TempDir(), "nope"), Options{Thickness: 4})
	if !errors.Is(err, ErrConnect) {
		t.Fatalf("got %v, want ErrConnect", err)
	}
}

func TestOpenViaEnvironment(t *testing.T) {
	s := startServer(t, fakewl.Options{})
	t.Setenv("XDG_RUNTIME_DIR", s.Dir())
	t.Setenv("WAYLAND_DISPLAY", s.Name())
	b, err := Open(Options{Thickness: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := s.WaitFor(wait, func(st fakewl.State) bool { return st.Layer.Acked }); err != nil {
		t.Fatal(err)
	}
}

func TestNoLayerShell(t *testing.T) {
	s := startServer(t, fakewl.Options{NoLayerShell: true})
	_, err := OpenAddr(s.Path(), Options{Thickness: 4})
	if !errors.Is(err, ErrNoLayerShell) {
		t.Fatalf("got %v, want ErrNoLayerShell", err)
	}
}

func TestLayerSurfaceSetup(t *testing.T) {
	s := startServer(t, fakewl.Options{})
	b := openBar(t, s, Options{Thickness: 4})
	if err := b.Draw(1, green); err != nil {
		t.Fatal(err)
	}
	lastFrame(t, s, func(fakewl.Frame) bool { return true })
	st := s.Snapshot()
	l := st.Layer
	if l.Namespace != "pomobar" || l.Layer != 3 || l.OutputID != 0 {
		t.Errorf("namespace/layer/output = %q/%d/%d, want pomobar/3(overlay)/0", l.Namespace, l.Layer, l.OutputID)
	}
	if l.Anchor != 2|4|8 {
		t.Errorf("anchor = %#x, want bottom|left|right", l.Anchor)
	}
	if l.Width != 0 || l.Height != 4 {
		t.Errorf("size = %dx%d, want 0x4", l.Width, l.Height)
	}
	if l.ExclusiveZone != 0 || l.KeyboardInteractivity != 0 {
		t.Errorf("exclusive zone %d, keyboard %d; want 0, 0", l.ExclusiveZone, l.KeyboardInteractivity)
	}
	if !st.InputRegionSet || len(st.InputRegion) != 0 {
		t.Errorf("input region set=%v rects=%v, want an empty region", st.InputRegionSet, st.InputRegion)
	}
	if !l.Acked || st.ProtocolError != "" {
		t.Errorf("acked=%v protocol error=%q", l.Acked, st.ProtocolError)
	}
}

func TestDrawShrinksTowardLeft(t *testing.T) {
	s := startServer(t, fakewl.Options{OutputWidth: 200})
	b := openBar(t, s, Options{Thickness: 4})
	if err := b.Draw(0.25, green); err != nil {
		t.Fatal(err)
	}
	f := lastFrame(t, s, func(f fakewl.Frame) bool { return f.Width == 200 })
	if f.Height != 4 || f.Format != 0 {
		t.Fatalf("frame %dx%d format %d", f.Width, f.Height, f.Format)
	}
	want := uint32(0xFF4CAF50)
	for y := range f.Height {
		for x := range f.Width {
			got := f.Pixel(x, y)
			exp := uint32(0)
			if x < 50 {
				exp = want
			}
			if got != exp {
				t.Fatalf("pixel (%d,%d) = %#08x, want %#08x", x, y, got, exp)
			}
		}
	}
}

func TestRedrawOnlyOnVisibleChange(t *testing.T) {
	s := startServer(t, fakewl.Options{OutputWidth: 100})
	b := openBar(t, s, Options{Thickness: 4})
	_ = b.Draw(1, green)
	lastFrame(t, s, func(fakewl.Frame) bool { return true })
	// 0.999 of 100 px rounds to 100 px: nothing visible changes.
	_ = b.Draw(0.999, green)
	_ = b.Draw(0.5, green)
	lastFrame(t, s, func(f fakewl.Frame) bool { return f.Pixel(50, 0) == 0 })
	if n := len(s.Snapshot().Frames); n != 2 {
		t.Fatalf("frames = %d, want 2", n)
	}
}

func TestBuffersAreReusedAfterRelease(t *testing.T) {
	s := startServer(t, fakewl.Options{OutputWidth: 100})
	b := openBar(t, s, Options{Thickness: 4})
	for i := 100; i >= 0; i-- {
		if err := b.Draw(float64(i)/100, green); err != nil {
			t.Fatal(err)
		}
	}
	lastFrame(t, s, func(f fakewl.Frame) bool { return f.Pixel(0, 0) == 0 })
	if st := s.Snapshot(); st.LiveBuffers != numBuffers || st.ProtocolError != "" {
		t.Fatalf("live buffers %d (want %d), protocol error %q", st.LiveBuffers, numBuffers, st.ProtocolError)
	}
}

func TestReconfigureResizes(t *testing.T) {
	s := startServer(t, fakewl.Options{OutputWidth: 100})
	b := openBar(t, s, Options{Thickness: 4})
	_ = b.Draw(0.5, green)
	lastFrame(t, s, func(f fakewl.Frame) bool { return f.Width == 100 })
	if err := s.Reconfigure(300, 4); err != nil {
		t.Fatal(err)
	}
	f := lastFrame(t, s, func(f fakewl.Frame) bool { return f.Width == 300 })
	if f.Pixel(149, 0) == 0 || f.Pixel(150, 0) != 0 {
		t.Fatal("bar not redrawn at the new size")
	}
	if _, err := s.WaitFor(wait, func(st fakewl.State) bool { return st.LiveBuffers == numBuffers }); err != nil {
		t.Fatalf("old buffers leaked: %v", err)
	}
}

func TestCompositorClosed(t *testing.T) {
	s := startServer(t, fakewl.Options{})
	b := openBar(t, s, Options{Thickness: 4})
	if _, err := s.WaitFor(wait, func(st fakewl.State) bool { return st.Layer.Acked }); err != nil {
		t.Fatal(err)
	}
	if err := s.SendClosed(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.Done():
	case <-time.After(wait):
		t.Fatal("Done not closed")
	}
	if !errors.Is(b.Err(), ErrClosed) {
		t.Fatalf("Err = %v, want ErrClosed", b.Err())
	}
	if err := b.Draw(0.5, green); !errors.Is(err, ErrClosed) {
		t.Fatalf("Draw after closed = %v", err)
	}
}

func TestConnectionLost(t *testing.T) {
	s, err := fakewl.Start(fakewl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	b := openBar(t, s, Options{Thickness: 4})
	s.Close()
	select {
	case <-b.Done():
	case <-time.After(wait):
		t.Fatal("Done not closed")
	}
	if b.Err() == nil || errors.Is(b.Err(), ErrClosed) {
		t.Fatalf("Err = %v", b.Err())
	}
}

func TestCloseRemovesBarAndSyncs(t *testing.T) {
	s := startServer(t, fakewl.Options{})
	b := openBar(t, s, Options{Thickness: 4})
	_ = b.Draw(0.5, green)
	lastFrame(t, s, func(fakewl.Frame) bool { return true })
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := s.WaitFor(wait, func(st fakewl.State) bool { return st.Disconnected })
	if err != nil {
		t.Fatal(err)
	}
	if !st.Layer.Destroyed || st.SurfaceAlive || st.LiveBuffers != 0 {
		t.Fatalf("after Close: layer destroyed=%v surface alive=%v buffers=%d", st.Layer.Destroyed, st.SurfaceAlive, st.LiveBuffers)
	}
	if st.SyncsAfterGone == 0 {
		t.Fatal("Close did not round-trip after destroying the surface")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestConcurrentDraw(t *testing.T) {
	s := startServer(t, fakewl.Options{OutputWidth: 500})
	b := openBar(t, s, Options{Thickness: 4})
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Go(func() {
			for i := range 200 {
				_ = b.Draw(float64((i*7+g)%100)/100, green)
			}
		})
	}
	wg.Wait()
	_ = b.Draw(0, green)
	lastFrame(t, s, func(f fakewl.Frame) bool { return f.Pixel(0, 0) == 0 })
	if st := s.Snapshot(); st.ProtocolError != "" {
		t.Fatal(st.ProtocolError)
	}
}

func TestEdges(t *testing.T) {
	const top, bottom, left, right = 1, 2, 4, 8
	cases := []struct {
		edge         Edge
		anchor       uint32
		reqW, reqH   uint32
		frameW, frmH int
	}{
		{Bottom, bottom | left | right, 0, 6, 200, 6},
		{Top, top | left | right, 0, 6, 200, 6},
		{Left, left | top | bottom, 6, 0, 6, 100},
		{Right, right | top | bottom, 6, 0, 6, 100},
	}
	for _, c := range cases {
		t.Run(c.edge.String(), func(t *testing.T) {
			s := startServer(t, fakewl.Options{OutputWidth: 200, OutputHeight: 100})
			b := openBar(t, s, Options{Edge: c.edge, Thickness: 6})
			if err := b.Draw(0.25, green); err != nil {
				t.Fatal(err)
			}
			f := lastFrame(t, s, func(fakewl.Frame) bool { return true })
			l := s.Snapshot().Layer
			if l.Anchor != c.anchor || l.Width != c.reqW || l.Height != c.reqH {
				t.Fatalf("anchor %#x size %dx%d, want %#x %dx%d", l.Anchor, l.Width, l.Height, c.anchor, c.reqW, c.reqH)
			}
			if f.Width != c.frameW || f.Height != c.frmH {
				t.Fatalf("frame %dx%d, want %dx%d", f.Width, f.Height, c.frameW, c.frmH)
			}
			// Horizontal bars keep the left quarter; vertical bars keep
			// the bottom quarter.
			for y := range f.Height {
				for x := range f.Width {
					lit := x < f.Width/4
					if c.edge.Vertical() {
						lit = y >= f.Height-f.Height/4
					}
					if (f.Pixel(x, y) != 0) != lit {
						t.Fatalf("pixel (%d,%d) lit=%v, want %v", x, y, f.Pixel(x, y) != 0, lit)
					}
				}
			}
		})
	}
}

func TestParseEdge(t *testing.T) {
	for _, e := range []Edge{Top, Bottom, Left, Right} {
		got, err := ParseEdge(e.String())
		if err != nil || got != e {
			t.Errorf("ParseEdge(%q) = %v, %v", e.String(), got, err)
		}
	}
	if _, err := ParseEdge("middle"); err == nil {
		t.Error("ParseEdge(middle) succeeded")
	}
}

func TestReserve(t *testing.T) {
	for _, reserve := range []bool{false, true} {
		s := startServer(t, fakewl.Options{})
		openBar(t, s, Options{Thickness: 5, Reserve: reserve})
		st, err := s.WaitFor(wait, func(st fakewl.State) bool { return st.Layer.Acked })
		if err != nil {
			t.Fatal(err)
		}
		want := int32(0)
		if reserve {
			want = 5
		}
		if st.Layer.ExclusiveZone != want {
			t.Errorf("reserve=%v: exclusive zone %d, want %d", reserve, st.Layer.ExclusiveZone, want)
		}
	}
}

func TestOutputScale(t *testing.T) {
	s := startServer(t, fakewl.Options{OutputWidth: 100, OutputScale: 2})
	b := openBar(t, s, Options{Thickness: 4})
	_ = b.Draw(0.5, green)
	f := lastFrame(t, s, func(fakewl.Frame) bool { return true })
	if f.Width != 200 || f.Height != 8 || f.BufferScale != 2 {
		t.Fatalf("frame %dx%d scale %d, want 200x8 scale 2", f.Width, f.Height, f.BufferScale)
	}
	if f.Pixel(99, 7) == 0 || f.Pixel(100, 0) != 0 {
		t.Fatal("half of the scaled buffer is not filled")
	}
}

func TestPreferredBufferScale(t *testing.T) {
	s := startServer(t, fakewl.Options{OutputWidth: 100, PreferredScale: 3})
	b := openBar(t, s, Options{Thickness: 4})
	_ = b.Draw(1, green)
	f := lastFrame(t, s, func(f fakewl.Frame) bool { return f.BufferScale == 3 })
	if f.Width != 300 || f.Height != 12 {
		t.Fatalf("frame %dx%d, want 300x12", f.Width, f.Height)
	}
	if st := s.Snapshot(); st.ProtocolError != "" {
		t.Fatal(st.ProtocolError)
	}
}

func TestColorChangeRedraws(t *testing.T) {
	s := startServer(t, fakewl.Options{OutputWidth: 100})
	b := openBar(t, s, Options{Thickness: 4})
	_ = b.Draw(0.5, green)
	lastFrame(t, s, func(fakewl.Frame) bool { return true })
	_ = b.Draw(0.5, color.RGBA{0x4D, 0xAF, 0x50, 0xFF}) // 1 step: invisible
	red := color.RGBA{0xE5, 0x39, 0x35, 0xFF}
	_ = b.Draw(0.5, red)
	lastFrame(t, s, func(f fakewl.Frame) bool { return f.Pixel(0, 0) == 0xFFE53935 })
	if n := len(s.Snapshot().Frames); n != 2 {
		t.Fatalf("frames = %d, want 2", n)
	}
}
