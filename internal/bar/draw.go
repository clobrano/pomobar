package bar

import (
	"encoding/binary"
	"image/color"
	"math"
)

// bytesPerPixel is the size of one ARGB8888 pixel.
const bytesPerPixel = 4

// filledPixels converts a fraction in [0, 1] into a number of whole pixels
// out of length, rounding to the nearest pixel.
func filledPixels(fraction float64, length int) int {
	if length <= 0 || math.IsNaN(fraction) || fraction <= 0 {
		return 0
	}
	if fraction >= 1 {
		return length
	}
	return int(math.Round(fraction * float64(length)))
}

// argb packs a premultiplied color.RGBA into a wl_shm ARGB8888 pixel value.
func argb(c color.RGBA) uint32 {
	return uint32(c.A)<<24 | uint32(c.R)<<16 | uint32(c.G)<<8 | uint32(c.B)
}

// fill paints a w×h ARGB8888 image (row stride in bytes) for a bar whose
// filled part is `filled` pixels long. Horizontal bars are filled from the
// left edge; vertical bars are filled from the bottom edge. The rest of the
// image is fully transparent (premultiplied zero).
func fill(pix []byte, w, h, stride int, vertical bool, filled int, c color.RGBA) {
	on := argb(c)
	for y := range h {
		row := pix[y*stride : y*stride+w*bytesPerPixel]
		for x := range w {
			var lit bool
			if vertical {
				lit = y >= h-filled
			} else {
				lit = x < filled
			}
			v := uint32(0)
			if lit {
				v = on
			}
			// wl_shm formats are little-endian.
			binary.LittleEndian.PutUint32(row[x*bytesPerPixel:], v)
		}
	}
}
