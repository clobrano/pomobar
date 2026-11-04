package timer

import (
	"image/color"
	"math"
)

// Bar colors.
var (
	Green = color.RGBA{0x4C, 0xAF, 0x50, 0xFF} // #4CAF50, more than 50 % left
	Amber = color.RGBA{0xFF, 0xB3, 0x00, 0xFF} // #FFB300, 50 % to 20 % left
	Red   = color.RGBA{0xE5, 0x39, 0x35, 0xFF} // #E53935, less than 20 % left
	Blue  = color.RGBA{0x42, 0xA5, 0xF5, 0xFF} // #42A5F5, pomodoro breaks
)

// Color thresholds, as remaining fractions.
const (
	AmberThreshold = 0.50
	RedThreshold   = 0.20
)

// BlendWidth is the width, as a fraction of the whole timer, of the
// transition below each threshold. The color is pure green down to 50 %,
// blends linearly to pure amber at 45 %, stays amber down to 20 %, and
// blends to pure red at 15 %. For a 25 m timer each blend lasts 75 s.
const BlendWidth = 0.05

// ColorStep is the smallest change of any 8-bit channel that counts as a
// visible color change (see VisiblyDifferent).
const ColorStep = 2

// Color returns the bar color for a remaining fraction in [0, 1].
func Color(fraction float64) color.RGBA {
	switch {
	case math.IsNaN(fraction):
		return Red
	case fraction >= AmberThreshold:
		return Green
	case fraction > AmberThreshold-BlendWidth:
		return lerp(Amber, Green, (fraction-(AmberThreshold-BlendWidth))/BlendWidth)
	case fraction >= RedThreshold:
		return Amber
	case fraction > RedThreshold-BlendWidth:
		return lerp(Red, Amber, (fraction-(RedThreshold-BlendWidth))/BlendWidth)
	default:
		return Red
	}
}

// lerp blends from a (t=0) to b (t=1) per sRGB channel.
func lerp(a, b color.RGBA, t float64) color.RGBA {
	t = min(max(t, 0), 1)
	mix := func(x, y uint8) uint8 {
		return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t))
	}
	return color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), mix(a.A, b.A)}
}

// VisiblyDifferent reports whether two colors differ by at least ColorStep
// in any channel.
func VisiblyDifferent(a, b color.RGBA) bool {
	d := func(x, y uint8) int {
		if x > y {
			return int(x - y)
		}
		return int(y - x)
	}
	return max(d(a.R, b.R), d(a.G, b.G), d(a.B, b.B), d(a.A, b.A)) >= ColorStep
}

// FilledLength converts a remaining fraction into a whole number of
// pixels out of length, rounding to the nearest pixel. Redraws happen only
// when this value (or the color) changes.
func FilledLength(fraction float64, length int) int {
	if length <= 0 || math.IsNaN(fraction) || fraction <= 0 {
		return 0
	}
	if fraction >= 1 {
		return length
	}
	return int(math.Round(fraction * float64(length)))
}

// Dim returns c at half opacity (premultiplied), used while paused.
func Dim(c color.RGBA) color.RGBA {
	return color.RGBA{c.R / 2, c.G / 2, c.B / 2, c.A / 2}
}
