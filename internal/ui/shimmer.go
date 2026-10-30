package ui

import (
	"math"
	"time"
)

// The editor mode's gold, and the light that sweeps across it.
var (
	editorGold = rgb{0xd4, 0xa0, 0x17}
	editorGlow = rgb{0xff, 0xf3, 0xc4}
)

// Yolo's red, and its light: the approval mode's label and web yolo's.
var (
	yoloRed  = rgb{0xe5, 0x48, 0x4d}
	yoloGlow = rgb{0xff, 0xd6, 0xc9}
)

// shimmerPeriod is how often the light crosses: it crosses in the first
// half and rests in the second.
const shimmerPeriod = 2400 * time.Millisecond

// shimmer writes text in base, bold, with a band of glow sweeping across
// it, like Claude Code's shimmering words.
func shimmer(text string, base, glow rgb, now time.Time) []span {
	runes := []rune(text)
	const band = 3.0 // the band's half width, in characters
	phase := float64(now.UnixNano()%int64(shimmerPeriod)) / float64(shimmerPeriod)
	center := phase*2*(float64(len(runes))+2*band) - band
	var out []span
	for i, r := range runes {
		w := max(0, 1-math.Abs(float64(i)-center)/band)
		l := look{fg: rgbInk(base.mix(glow, w)), bold: true}
		if n := len(out); n > 0 && out[n-1].look == l {
			out[n-1].text += string(r)
			continue
		}
		out = append(out, span{text: string(r), look: l})
	}
	return out
}
