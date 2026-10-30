package agent

import (
	"bytes"
	"hash/maphash"
	"regexp"
	"strings"
)

// chantDetector notices a reply that keeps repeating the same text, a
// failure mode of small local models that otherwise runs until the output
// limit. It is ported from the content-loop check of Qwen Code's
// packages/core/src/services/loopDetectionService.ts: every 50-byte chunk
// of the last 4000 bytes is tracked, and a reply is looping when one chunk
// occurs 10 times with an average spacing of at most 75 bytes, or when a
// run of at least 5 equally spaced occurrences spans 1000 bytes or more
// that repeat exactly. Markdown structure (code blocks, tables, lists,
// headings, quotes, dividers) resets the tracking, because it repeats
// legitimately, and code blocks aren't checked at all.
type chantDetector struct {
	buf    []byte
	next   int              // the next chunk position to look at
	seen   map[uint64][]int // where each chunk occurs, by hash
	seed   maphash.Seed
	inCode bool
}

const (
	chantChunk        = 50
	chantThreshold    = 10
	chantWindow       = 4000
	chantSlack        = 1000 // the buffer is trimmed in steps of this much
	periodicRun       = 5
	periodicShortRun  = 3 // when earlier occurrences fell out of the window
	periodicMinRegion = 1000
)

var (
	tableRE      = regexp.MustCompile(`(^|\n)\s*(\|.*\||[|+-]{3,})`)
	listItemRE   = regexp.MustCompile(`(^|\n)\s*([-*+]|\d+\.)\s`)
	headingRE    = regexp.MustCompile(`(^|\n)#+\s`)
	blockquoteRE = regexp.MustCompile(`(^|\n)>\s`)
	dividerRE    = regexp.MustCompile(`^[-+_=*\x{2500}-\x{257F}]+$`)
)

// text takes a piece of the visible reply and reports whether the reply
// is looping.
func (d *chantDetector) text(s string) bool {
	fences := strings.Count(s, "```")
	divider := dividerRE.MatchString(s)
	if fences > 0 || divider || tableRE.MatchString(s) || listItemRE.MatchString(s) ||
		headingRE.MatchString(s) || blockquoteRE.MatchString(s) {
		d.reset()
	}
	wasInCode := d.inCode
	if fences%2 == 1 {
		d.inCode = !d.inCode
	}
	if wasInCode || d.inCode || divider {
		return false
	}
	return d.add(s)
}

// reasoning takes a piece of the reasoning, which isn't Markdown.
func (d *chantDetector) reasoning(s string) bool { return d.add(s) }

func (d *chantDetector) reset() {
	d.buf, d.next = d.buf[:0], 0
	clear(d.seen)
}

func (d *chantDetector) add(s string) bool {
	if d.seen == nil {
		d.seen, d.seed = map[uint64][]int{}, maphash.MakeSeed()
	}
	d.buf = append(d.buf, s...)
	d.trim()
	for ; d.next+chantChunk <= len(d.buf); d.next++ {
		if d.loopAt(d.next) {
			return true
		}
	}
	return false
}

// windowStart is where the window of tracked text begins in buf.
func (d *chantDetector) windowStart() int { return max(0, len(d.buf)-chantWindow) }

func (d *chantDetector) trim() {
	if len(d.buf) <= chantWindow+chantSlack {
		return
	}
	cut := len(d.buf) - chantWindow
	d.buf = append(d.buf[:0], d.buf[cut:]...)
	d.next = max(0, d.next-cut)
	for h, pos := range d.seen {
		kept := pos[:0]
		for _, p := range pos {
			if p >= cut {
				kept = append(kept, p-cut)
			}
		}
		if len(kept) == 0 {
			delete(d.seen, h)
		} else {
			d.seen[h] = kept
		}
	}
}

// loopAt records the chunk at pos and checks the repetition rules.
func (d *chantDetector) loopAt(pos int) bool {
	chunk := d.buf[pos : pos+chantChunk]
	h := maphash.Bytes(d.seed, chunk)
	occ := d.seen[h]
	for start := d.windowStart(); len(occ) > 0 && occ[0] < start; {
		occ = occ[1:]
	}
	if len(occ) == 0 {
		d.seen[h] = []int{pos}
		return false
	}
	if !bytes.Equal(d.buf[occ[0]:occ[0]+chantChunk], chunk) {
		d.seen[h] = occ // a hash collision
		return false
	}
	occ = append(occ, pos)
	d.seen[h] = occ
	return d.clustered(occ) || d.periodic(occ)
}

// clustered: the chunk's last occurrences are close together, so the
// repeated unit is short.
func (d *chantDetector) clustered(occ []int) bool {
	if len(occ) < chantThreshold {
		return false
	}
	recent := occ[len(occ)-chantThreshold:]
	spacing := float64(recent[len(recent)-1]-recent[0]) / float64(chantThreshold-1)
	return spacing <= 1.5*chantChunk
}

// periodic: a long unit repeats verbatim, so its chunks recur at exactly
// equal spacing, and the text they span repeats with that period.
func (d *chantDetector) periodic(occ []int) bool {
	if len(occ) < periodicShortRun {
		return false
	}
	last := len(occ) - 1
	stride := occ[last] - occ[last-1]
	first := last
	for first > 0 && occ[first]-occ[first-1] == stride {
		first--
	}
	switch run := last - first + 1; {
	case run >= periodicRun:
		return d.repeatsWithPeriod(occ[first], occ[last]+chantChunk, stride)
	case run >= periodicShortRun && len(d.buf) >= chantWindow:
		return d.repeatsWithPeriod(d.windowStart(), occ[last]+chantChunk, stride)
	}
	return false
}

func (d *chantDetector) repeatsWithPeriod(start, end, stride int) bool {
	if end-start < periodicMinRegion {
		return false
	}
	for i := start; i+stride < end; i++ {
		if d.buf[i] != d.buf[i+stride] {
			return false
		}
	}
	return true
}
