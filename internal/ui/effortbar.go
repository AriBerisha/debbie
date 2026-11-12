package ui

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"debbie/internal/effort"
)

// The /effort slider, after Claude Code's: ← and → move the knob along the
// levels the model supports, Enter saves the level as the provider's
// default, s applies it to this session only, and Esc leaves the effort as
// it was. Sparkles gather around the knob as the level rises, and from
// xhigh up the knob glows. The last stop, ultraworkflow, coordinates several
// agents at once and has a rainbow track.

// effortStop is a stop on the slider.
type effortStop struct {
	name     string
	level    effort.Level
	about    string
	heat     int  // 0 to 6: the sparkles around the knob, and from 4 its glow
	workflow bool // ultraworkflow's rainbow presentation
	tokens   int  // the auto-compaction slider's stops: the context size
}

var effortAbout = map[effort.Level]string{
	effort.None:    "no reasoning, the fastest replies",
	effort.Minimal: "barely thinks first",
	effort.Low:     "a quick think before answering",
	effort.Medium:  "thinks it through",
	effort.High:    "thinks hard",
	effort.XHigh:   "thinks hardest, slower",
	effort.Max:     "thinks without limit, slowest",
}

var effortHeat = map[effort.Level]int{
	effort.Minimal: 1, effort.Low: 1, effort.Medium: 2, effort.High: 3, effort.XHigh: 4, effort.Max: 5,
}

// effortStops lists the stops: none, the model's levels (or the usual ones
// when they are unknown), and ultraworkflow.
func effortStops(levels []effort.Level) []effortStop {
	if len(levels) == 0 {
		levels = []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max}
	}
	stops := []effortStop{{name: string(effort.None), level: effort.None, about: effortAbout[effort.None]}}
	for _, l := range levels {
		if l != effort.None && l != effort.Default {
			stops = append(stops, effortStop{name: string(l), level: l, about: effortAbout[l], heat: effortHeat[l]})
		}
	}
	return append(stops, effortStop{name: string(effort.UltraWorkflow), level: effort.UltraWorkflow,
		about: "several agents working at once", heat: 6, workflow: true})
}

// effortSlider is the slider's state.
type effortSlider struct {
	title   string // the heading; "" for the effort's
	stops   []effortStop
	pos     int    // the stop the knob is on
	now     int    // the stop in effect when the slider opened, or -1
	nowName string // the effort in effect when the slider opened
	msg     string // shown instead of the stop's description until the knob moves
}

func newEffortSlider(levels []effort.Level, current effort.Level) *effortSlider {
	s := &effortSlider{stops: effortStops(levels), now: -1, nowName: "the server's default"}
	if current == effort.UltraWorkflow {
		s.nowName = string(current)
		s.now = len(s.stops) - 1
	} else if current != effort.Default {
		sent := effort.Fit(current, levels)
		s.nowName = string(sent)
		for i, st := range s.stops {
			if !st.workflow && st.level == sent {
				s.now = i
			}
		}
	}
	s.pos = s.now
	if s.pos < 0 {
		s.pos = (len(s.stops) - 1) / 2
	}
	return s
}

// key applies a key press. It reports whether the user is done: chose a
// stop (save tells whether as the default) or left the slider (ok false).
func (s *effortSlider) key(p keyPress) (done, ok, save bool) {
	last := len(s.stops) - 1
	move := func(to int) {
		if to = max(0, min(last, to)); to != s.pos {
			s.pos, s.msg = to, ""
		}
	}
	switch {
	case p.k == keyLeft || p.k == keyRune && strings.ContainsRune("h-", p.r):
		move(s.pos - 1)
	case p.k == keyRight || p.k == keyRune && strings.ContainsRune("l+=", p.r):
		move(s.pos + 1)
	case p.k == keyHome:
		move(0)
	case p.k == keyEnd:
		move(last)
	case p.k == keyEnter || p.k == keyRune && (p.r == 's' || p.r == 'S'):
		return true, true, p.k == keyEnter
	case p.k == keyEsc || p.k == keyRune && (p.r == 'q' || p.r == 'Q'):
		return true, false, false
	}
	return false, false, false
}

// Colors of the slider.
var (
	tealDim    = rgb{0x2a, 0x8a, 0x75}
	tealBright = rgb{0x9f, 0xf5, 0xdd}
	mint       = rgb{0xe6, 0xff, 0xf8}
	gold       = rgb{0xff, 0xc9, 0x4d}
	warmWhite  = rgb{0xff, 0xf6, 0xdc}
	railGray   = rgb{0x40, 0x40, 0x40}
)

// cell is a character on the slider's canvas and its look.
type cell struct {
	r    rune
	look look
}

// cellSpans turns cells into spans, joining neighbors with the same look.
func cellSpans(cells []cell) []span {
	var spans []span
	for _, c := range cells {
		if n := len(spans); n > 0 && spans[n-1].look == c.look {
			spans[n-1].text += string(c.r)
			continue
		}
		spans = append(spans, span{string(c.r), c.look})
	}
	return spans
}

// encodeSpans writes spans as text with escape sequences, for line mode:
// the looks only with color on, and no spaces at the end.
func encodeSpans(spans []span, t Terminal) string {
	var b strings.Builder
	for _, sp := range spans {
		if t.Color && sp.look != (look{}) {
			b.WriteString(sp.look.sgr(true, t.TrueColor) + sp.text + "\x1b[0m")
			continue
		}
		b.WriteString(sp.text)
	}
	return strings.TrimRight(b.String(), " ")
}

// hash mixes numbers into a pseudo-random one (SplitMix64's finalizer), so a
// frame always looks the same.
func hash(xs ...int) uint64 {
	h := uint64(0x9e3779b97f4a7c15)
	for _, x := range xs {
		h ^= uint64(x) + 0x9e3779b97f4a7c15 + h<<6 + h>>2
		h ^= h >> 30
		h *= 0xbf58476d1ce4e5b9
		h ^= h >> 27
		h *= 0x94d049bb133111eb
		h ^= h >> 31
	}
	return h
}

// hue returns a bright color of the rainbow, h in degrees.
func hue(h float64) rgb {
	h = math.Mod(h, 360) / 60
	x := 1 - math.Abs(math.Mod(h, 2)-1)
	var r, g, b float64
	switch int(h) {
	case 0:
		r, g = 1, x
	case 1:
		r, g = x, 1
	case 2:
		g, b = 1, x
	case 3:
		g, b = x, 1
	case 4:
		r, b = x, 1
	default:
		r, b = 1, x
	}
	lift := func(v float64) uint8 { return uint8(255 * (0.45 + 0.55*v)) } // pastel, readable on dark
	return rgb{lift(r), lift(g), lift(b)}
}

// The sparkles' twinkle: each lives sparkleLife frames, brightening then
// fading through one of these shapes, and then appears somewhere else.
// (Console fonts draw · and ˚ too small to see from afar.)
var twinkles = [][]rune{
	[]rune("∙•*•∙"),
	[]rune("+*+"),
	[]rune("∙•∙"),
	[]rune("·+*+·"),
}

const sparkleLife = 8

// lines draws the slider at an animation frame in line mode, for a
// terminal cols columns wide.
func (s *effortSlider) lines(t Terminal, frame, cols int) []string {
	var lines []string
	for _, row := range s.rows(t, frame, cols) {
		lines = append(lines, encodeSpans(row, t))
	}
	return lines
}

// rows draws the slider at an animation frame as rows of spans.
func (s *effortSlider) rows(t Terminal, frame, cols int) [][]span {
	paint := func(c rgb) look { return look{fg: rgbInk(c)} }
	// The canvas holds the bar and, centered under each stop, its label;
	// the first and last labels hang past the ends of the bar.
	const indent = "  "
	n := len(s.stops)
	first, last := len([]rune(s.stops[0].name)), len([]rune(s.stops[n-1].name))
	left, right := first/2, last-1-last/2
	barWidth := max(10, min(60, cols-len(indent)-left-right-2))
	width := left + barWidth + right
	xs := make([]int, n) // the stops' columns
	for i := range xs {
		xs[i] = left + int(math.Round(float64(i)*float64(barWidth-1)/float64(n-1)))
	}
	stop := s.stops[s.pos]
	knob := xs[s.pos]
	pulse := 0.5 + 0.5*math.Sin(2*math.Pi*float64(frame)/16)
	accent := func() rgb { // the color of the chosen stop's name and knob
		switch {
		case stop.workflow:
			return hue(float64(frame) * 12)
		case stop.heat >= 4:
			return gold.mix(warmWhite, pulse)
		case stop.heat == 0:
			return railGray.mix(mint, 0.4)
		}
		return mint
	}()

	// The bar: a half-height track, lit up to the knob, which stands up
	// from it full height; from xhigh up, the knob glows and warms the
	// track around it. Only half and full blocks: every console font has
	// them.
	bar := blankCells(width)
	glow := 0.0
	if stop.heat >= 4 {
		glow = 6 + 4*float64(stop.heat-4)
	}
	for x := left; x < left+barWidth; x++ {
		var c rgb
		switch {
		case x < knob && stop.workflow:
			c = hue(float64(x-left)*360/float64(barWidth) + float64(frame)*12)
		case x < knob:
			c = logoFrom.mix(tealBright, float64(x-left)/float64(max(1, knob-left)))
		default:
			c = railGray
		}
		if d := math.Abs(float64(x - knob)); glow > 0 && d < glow {
			f := 1 - d/glow
			if x < knob {
				c = c.mix(warmWhite, f*(0.3+0.5*pulse))
			} else {
				c = c.mix(gold, f*f*(0.25+0.45*pulse))
			}
		}
		bar[x] = cell{'▄', paint(c)}
	}
	bar[knob] = cell{'█', paint(accent)}

	// Sparkles above and below the bar, more and farther out as the level
	// rises; the rainbow ones of ultraworkflow cover the whole bar.
	above, below := blankCells(width), blankCells(width)
	count := []int{0, 2, 4, 6, 12, 16, 24}[stop.heat]
	reach := []int{0, 4, 6, 9, 12, 15, width}[stop.heat]
	for j := range count {
		age := frame + j*5
		life, phase := age/sparkleLife, age%sparkleLife
		h := hash(j, life, s.pos)
		shape := twinkles[h%uint64(len(twinkles))]
		if phase >= len(shape) {
			continue // between lives
		}
		dx := int((h>>8)%uint64(2*reach)) - reach
		if dx >= 0 {
			dx++ // never on the knob's column
		}
		x := knob + dx
		if x < 0 || x >= width {
			continue
		}
		row := above
		if (h>>20)%2 == 1 {
			row = below
		}
		if row[x].r != ' ' {
			continue
		}
		bright := math.Sin(math.Pi * float64(phase+1) / float64(len(shape)+1))
		var c rgb
		switch {
		case stop.workflow:
			c = railGray.mix(hue(float64(h%360)+float64(frame)*9), 0.4+0.6*bright)
		case stop.heat >= 4 && (h>>24)%3 != 0:
			c = gold.mix(warmWhite, bright).mix(railGray, 0.5*(1-bright))
		default:
			c = tealDim.mix(mint, bright)
		}
		row[x] = cell{shape[phase], paint(c)}
	}

	// The labels under their stops, the chosen one lit; when they don't fit,
	// only the chosen one.
	labels := blankCells(width)
	place := func(i int, all bool) bool {
		name := []rune(s.stops[i].name)
		start := max(0, min(width-len(name), xs[i]-len(name)/2))
		if all {
			for x := max(0, start-1); x < min(width, start+len(name)+1); x++ {
				if labels[x].r != ' ' {
					return false
				}
			}
		}
		l := look{fg: inkGray}
		if i == s.pos {
			l = look{fg: rgbInk(accent), bold: true}
		}
		for k, r := range name {
			if start+k < width {
				labels[start+k] = cell{r, l}
			}
		}
		return true
	}
	fits := true
	for i := range s.stops {
		if fits = place(i, true); !fits {
			break
		}
	}
	if !fits {
		labels = blankCells(width)
		place(s.pos, false)
	}

	// No line may wrap: line mode redraws the slider in place by moving up
	// a line for each line drawn.
	room := cols - len(indent) - 1
	row := func(cells []cell) []span {
		return append([]span{plain(indent)}, cellSpans(trimCells(cells[:min(len(cells), max(0, room))]))...)
	}
	heading := s.title
	if heading == "" {
		heading = "Reasoning effort"
	}
	title := []span{plain(indent), styled(clip(heading, room), look{bold: true})}
	if now := "   now " + s.nowName; len(heading)+len(now) <= room {
		title = append(title, grayed(now))
	}
	name := clip(stop.name, room)
	desc := []span{plain(indent), styled(name, look{fg: rgbInk(accent), bold: true})}
	if left := room - utf8.RuneCountInString(name); left > 3 && stop.about != "" {
		desc = append(desc, grayed(clip(" · "+stop.about, left)))
	}
	if s.msg != "" {
		desc = []span{plain(indent), inked(clip(s.msg, room), inkYellow)}
	}
	hints := "← → choose · Enter save · s this session only · Esc cancel"
	if utf8.RuneCountInString(hints) > room {
		hints = "← → · Enter save · s session · Esc"
	}
	return [][]span{
		title,
		row(above),
		row(bar),
		row(below),
		row(labels),
		desc,
		{plain(indent), grayed(clip(hints, room))},
	}
}

// trimCells drops blank cells from the end of a row.
func trimCells(cells []cell) []cell {
	for len(cells) > 0 && cells[len(cells)-1].r == ' ' && cells[len(cells)-1].look == (look{}) {
		cells = cells[:len(cells)-1]
	}
	return cells
}

func blankCells(n int) []cell {
	cells := make([]cell, n)
	for i := range cells {
		cells[i] = cell{r: ' '}
	}
	return cells
}

// clip shortens s to n runes.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:max(0, n-1)]) + "…"
	}
	return s
}

// clipLeft shortens s to n characters from the left, keeping its end.
func clipLeft(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return "…" + string(r[len(r)-max(0, n-1):])
	}
	return s
}

// minSliderColumns is the narrowest terminal the slider is drawn in;
// narrower ones get the plain text.
const minSliderColumns = 30

// effortTick is how often the slider's sparkles move.
var effortTick = 110 * time.Millisecond

// pickEffort runs the slider until the user chooses a level or leaves.
func (c *chat) pickEffort(s *effortSlider, kb keyboard) (choice effortStop, ok, save, quit bool) {
	drawn := 0
	up := func(n int) string { // CUU with 0 would move one row
		if n <= 0 {
			return "\r"
		}
		return fmt.Sprintf("\r\x1b[%dA", n)
	}
	frame := 0
	draw := func() {
		lines := s.lines(c.term, frame, c.term.columns())
		var b strings.Builder
		if drawn > 0 {
			b.WriteString(up(drawn - 1))
		}
		b.WriteString(strings.Join(lines, "\x1b[K\n") + "\x1b[K")
		drawn = len(lines)
		io.WriteString(c.out, b.String())
		c.out.Flush()
	}
	io.WriteString(c.out, "\x1b[?25l") // the cursor would blink at the end of the hints
	defer func() {
		io.WriteString(c.out, up(drawn-1)+"\x1b[J\x1b[?25h")
		c.out.Flush()
	}()
	tick := time.NewTicker(effortTick)
	defer tick.Stop()
	draw()
	for {
		select {
		case <-c.quit:
			return effortStop{}, false, false, true
		case <-c.interrupts:
			return effortStop{}, false, false, false
		case <-tick.C:
			frame++
			draw()
		case ev, open := <-kb.events():
			if !open {
				return effortStop{}, false, false, false
			}
			if ev.kind != evKey {
				continue
			}
			if done, ok, save := s.key(ev.key); done {
				return s.stops[s.pos], ok, save, false
			}
			draw()
		}
	}
}
