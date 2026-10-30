package ui

import (
	"fmt"
	"slices"

	"debbie/internal/agent"
)

// The /autocompact slider: the effort slider's track, with stops at
// shares of the model's context window (never at the left), where the
// conversation is summarized to make room. Enter saves the stop for the
// provider in use, s keeps it to this session, Esc leaves it.

// autoCompactStops lists the stops: never; then, sorted, shares of the
// context window (or round amounts when it is unknown), the usual size
// and the one in effect.
func autoCompactStops(window, current, usual int) []effortStop {
	var tokens []int
	add := func(n int) {
		if n > 0 && !slices.Contains(tokens, n) {
			tokens = append(tokens, n)
		}
	}
	if window > 0 {
		for _, share := range []float64{0.25, 0.4, 0.5, 0.6, 0.7, 0.8, 0.85} {
			n := int(float64(window) * share)
			if n >= 10_000 {
				n = n / 1000 * 1000 // round thousands, as the labels show them
			}
			add(n)
		}
	} else {
		for _, n := range []int{50_000, 100_000, 150_000, 180_000, 250_000, 400_000, 600_000, 800_000} {
			add(n)
		}
	}
	add(usual)
	add(current)
	slices.Sort(tokens)
	stops := []effortStop{{name: "never", about: "/compact compacts it when you ask"}}
	for i, n := range tokens {
		var about []string
		if window > 0 {
			about = append(about, fmt.Sprintf("%d%% of the %s context", (n*100+window/2)/window, kTokens(window)))
		}
		if n == usual {
			about = append(about, "the usual")
		}
		if n == current {
			about = append(about, "as it is now")
		}
		// Hotter the later it compacts: more context kept, and a gold knob
		// at the top.
		heat := 1 + i*4/max(1, len(tokens)-1)
		stops = append(stops, effortStop{name: kTokens(n), about: joinAbout(about), heat: heat, tokens: n})
	}
	return stops
}

func joinAbout(parts []string) string {
	s := ""
	for i, p := range parts {
		if i > 0 {
			s += " · "
		}
		s += p
	}
	return s
}

// newAutoCompactSlider makes the slider for a model whose context window
// is window (0 when unknown), with current the setting in effect.
func newAutoCompactSlider(window, current int) *effortSlider {
	usual := agent.AutoCompactAt(agent.DefaultAutoCompact, window)
	current = agent.AutoCompactAt(current, window)
	s := &effortSlider{title: "Auto-compaction", stops: autoCompactStops(window, current, usual), nowName: "off"}
	for i, st := range s.stops {
		if st.tokens == current && current > 0 {
			s.now, s.nowName = i, "at "+st.name+" tokens"
		}
	}
	s.pos = s.now
	return s
}

// autoCompactWords says what a stop sets: "off", or "at 850k tokens".
func autoCompactWords(stop effortStop) string {
	if stop.tokens == 0 {
		return "off"
	}
	return "at " + stop.name + " tokens"
}

// applyAutoCompact puts a chosen stop into effect, saved for the provider
// in use when save is set, and says what happened.
func (c *chat) applyAutoCompact(stop effortStop, save bool) []span {
	c.opts.AutoCompact = stop.tokens
	line := []span{plain("Auto-compaction: " + autoCompactWords(stop))}
	switch {
	case !save:
		line = append(line, grayed(", for this session"))
	case c.opts.Providers == nil:
		line = append(line, grayed(", for this session (there is no config file to save it in)"))
	default:
		if err := c.opts.Providers.SetAutoCompact(c.opts.Status.Provider, stop.tokens); err != nil {
			line = append(line, inked(", for this session; not saved: "+err.Error(), inkYellow))
		} else {
			line = append(line, grayed(", saved for "+c.opts.Status.Provider))
		}
	}
	return line
}

// The fullscreen UI.

// compactPanel is the /autocompact slider.
type compactPanel struct {
	s *effortSlider
}

func (p *compactPanel) rows(f *fullscreen, width int) [][]span {
	return append([][]span{rule(width)}, p.s.rows(f.term, f.frame, width)...)
}

func (p *compactPanel) key(f *fullscreen, k keyPress) bool {
	done, ok, save := p.s.key(k)
	if !done {
		return false
	}
	if !ok {
		f.say("Auto-compaction stays "+p.s.nowName+".", inkGray)
		return true
	}
	f.tr.add(&noteBlock{spans: f.chat.applyAutoCompact(p.s.stops[p.s.pos], save)})
	f.scroll = 0
	return true
}

func (p *compactPanel) click(f *fullscreen, row int) bool { return false }

// Line mode.

// autoCompactSlider lets the user pick on the slider, as effortSlider
// does for the effort.
func (c *chat) autoCompactSlider(kb keyboard) (quit bool) {
	s := newAutoCompactSlider(c.opts.Status.ContextWindow, c.opts.AutoCompact)
	choice, ok, save, quit := c.pickEffort(s, kb)
	switch {
	case quit:
		return true
	case !ok:
		c.r.println(c.st.gray("Auto-compaction stays " + s.nowName + "."))
		return false
	}
	c.r.println(encodeSpans(c.applyAutoCompact(choice, save), c.term))
	return false
}
