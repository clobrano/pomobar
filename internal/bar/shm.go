package bar

import (
	"fmt"

	"github.com/vcaesar/go-wayland/client"
	"golang.org/x/sys/unix"
)

// numBuffers is how many buffers each pool holds. Two lets us draw the next
// frame while the compositor still reads the previous one.
const numBuffers = 2

// shmBuffer is one wl_buffer backed by a slice of the pool's mapping.
type shmBuffer struct {
	wl   *client.Buffer
	pix  []byte
	busy bool // attached and not yet released by the compositor
}

// shmPool is a memfd-backed wl_shm_pool holding numBuffers buffers of one
// size. A pool is replaced, not resized, when the surface size changes.
type shmPool struct {
	wl            *client.ShmPool
	mem           []byte
	width, height int // buffer size in pixels
	stride        int
	bufs          [numBuffers]*shmBuffer
}

// newShmPool allocates a pool for buffers of w×h pixels. onRelease is called
// (on the dispatch goroutine) when the compositor releases a buffer.
// The caller must hold the lock that serializes object-creating requests.
func newShmPool(shm *client.Shm, w, h int, onRelease func(*shmBuffer)) (*shmPool, error) {
	stride := w * bytesPerPixel
	bufSize := stride * h
	size := bufSize * numBuffers
	if w <= 0 || h <= 0 || size > 1<<30 {
		return nil, fmt.Errorf("invalid buffer size %dx%d", w, h)
	}

	fd, err := unix.MemfdCreate("pomobar", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("memfd_create: %w", err)
	}
	// The compositor receives its own copy of the fd, so ours can be
	// closed as soon as create_pool has been sent.
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		return nil, fmt.Errorf("ftruncate: %w", err)
	}
	// Forbid shrinking so the compositor can never SIGBUS on our pool.
	_, _ = unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL)
	mem, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}

	p := &shmPool{mem: mem, width: w, height: h, stride: stride}
	p.wl, err = shm.CreatePool(fd, int32(size))
	if err != nil {
		_ = unix.Munmap(mem)
		return nil, fmt.Errorf("wl_shm.create_pool: %w", err)
	}
	for i := range p.bufs {
		off := i * bufSize
		b := &shmBuffer{pix: mem[off : off+bufSize]}
		b.wl, err = p.wl.CreateBuffer(int32(off), int32(w), int32(h), int32(stride), uint32(client.ShmFormatArgb8888))
		if err != nil {
			p.destroy()
			return nil, fmt.Errorf("wl_shm_pool.create_buffer: %w", err)
		}
		b.wl.SetReleaseHandler(func(client.BufferReleaseEvent) { onRelease(b) })
		publish(b.wl)
		p.bufs[i] = b
	}
	return p, nil
}

// free returns a buffer the compositor is not reading, or nil.
func (p *shmPool) free() *shmBuffer {
	for _, b := range p.bufs {
		if b != nil && !b.busy {
			return b
		}
	}
	return nil
}

// destroy sends the destroy requests and unmaps our view of the memory.
// The compositor keeps its own mapping for as long as it needs it.
func (p *shmPool) destroy() {
	for _, b := range p.bufs {
		if b != nil && b.wl != nil {
			_ = b.wl.Destroy()
		}
	}
	if p.wl != nil {
		_ = p.wl.Destroy()
	}
	if p.mem != nil {
		_ = unix.Munmap(p.mem)
		p.mem = nil
	}
}

// publish makes handler fields set on p visible to the dispatch goroutine.
// The generated proxies store handlers in plain fields that Dispatch reads
// without locking; client.Context guards its object table with a mutex that
// Dispatch also takes, so a lookup here orders our writes before any event
// for p is dispatched.
func publish(p client.Proxy) {
	_ = p.Context().GetProxy(p.ID())
}
