package ui

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"debbie/internal/setup"
)

// picker is a list of options picked with the keys, as in Claude Code: ↑
// and ↓ move, Enter picks, a number picks at once. With multi, Space ticks
// options and Enter takes the ticked ones. The Other entry, when there is
// one, takes an answer typed into it; typing anywhere starts it.
type picker struct {
	title    string // a line above the question, such as "Database (1 of 2)"; "" for none
	question string
	options  []setup.Option
	other    string // the Other entry's label; "" for none
	multi    bool
	// detailBelow puts each option's detail on a line of its own, for
	// details longer than a few words.
	detailBelow bool
	sel         int // the entry the cursor is on; len(options) for Other
	ticked      []bool
	typed       []rune
}

func newPicker(question string, options []setup.Option, def int, other string, multi bool) *picker {
	p := &picker{question: question, options: options, other: other, multi: multi, ticked: make([]bool, len(options))}
	p.sel = max(0, min(def, p.count()-1))
	return p
}

// count is the number of entries, Other included.
func (p *picker) count() int {
	if p.other != "" {
		return len(p.options) + 1
	}
	return len(p.options)
}

func (p *picker) onOther() bool { return p.other != "" && p.sel == len(p.options) }

// pickStatus is where a pick stands after a key.
type pickStatus int

const (
	picking pickStatus = iota
	picked
	pickCancelled
)

func (p *picker) key(k keyPress) pickStatus {
	n := p.count()
	switch {
	case k.k == keyEsc || k.ctrl('c'):
		return pickCancelled
	case k.k == keyUp || k.k == keyBackTab:
		p.sel = (p.sel - 1 + n) % n
	case k.k == keyDown || k.k == keyTab:
		p.sel = (p.sel + 1) % n
	case k.k == keyHome:
		p.sel = 0
	case k.k == keyEnd:
		p.sel = n - 1
	case k.k == keyEnter:
		return p.enter()
	case p.onOther() && k.k == keyBackspace:
		if len(p.typed) > 0 {
			p.typed = p.typed[:len(p.typed)-1]
		}
	case p.onOther() && k.ctrl('u'):
		p.typed = nil
	case k.k != keyRune || k.mod&(modCtrl|modAlt) != 0:
	case p.onOther():
		p.typed = append(p.typed, k.r)
	case k.r == ' ' && p.multi:
		p.ticked[p.sel] = !p.ticked[p.sel]
	case k.r >= '1' && k.r <= '9' && int(k.r-'0') <= n:
		p.sel = int(k.r - '1')
		switch {
		case p.onOther():
		case p.multi:
			p.ticked[p.sel] = !p.ticked[p.sel]
		default:
			return picked
		}
	case p.other != "" && k.r > ' ':
		p.sel = len(p.options) // typing starts the answer of one's own
		p.typed = append(p.typed, k.r)
	}
	return picking
}

func (p *picker) enter() pickStatus {
	typed := strings.TrimSpace(string(p.typed)) != ""
	switch {
	case p.onOther() && typed:
		return picked
	case p.onOther() && p.multi && p.anyTicked():
		return picked
	case p.onOther():
		return picking // nothing typed yet
	case p.multi && !p.anyTicked():
		p.ticked[p.sel] = true
	}
	return picked
}

func (p *picker) anyTicked() bool {
	for _, t := range p.ticked {
		if t {
			return true
		}
	}
	return false
}

// paste types pasted text into the Other entry, on one line: each run of
// spaces and line breaks becomes a single space.
func (p *picker) paste(s string) {
	if p.other == "" {
		return
	}
	p.sel = len(p.options)
	space := len(p.typed) > 0 && p.typed[len(p.typed)-1] == ' '
	for _, r := range s {
		switch {
		case !unicode.IsSpace(r):
			p.typed = append(p.typed, r)
			space = false
		case !space && len(p.typed) > 0:
			p.typed = append(p.typed, ' ')
			space = true
		}
	}
}

// answer is what was picked: the options' indexes, and the text typed into
// Other ("" for none).
func (p *picker) answer() (indexes []int, typed string) {
	typed = strings.TrimSpace(string(p.typed))
	if !p.multi {
		if p.onOther() {
			return nil, typed
		}
		return []int{p.sel}, ""
	}
	for i, t := range p.ticked {
		if t {
			indexes = append(indexes, i)
		}
	}
	return indexes, typed
}

// summary is the answer in words, for the line left behind.
func (p *picker) summary() string {
	idx, typed := p.answer()
	var parts []string
	for _, i := range idx {
		parts = append(parts, p.options[i].Label)
	}
	if typed != "" {
		parts = append(parts, typed)
	}
	return strings.Join(parts, ", ")
}

// hint says which keys work.
func (p *picker) hint() string {
	switch {
	case p.onOther():
		return "type your answer · Enter to send · ↑↓ to move · Esc to cancel"
	case p.multi:
		return "↑↓ to move · Space to tick · Enter when done · Esc to cancel"
	}
	return "↑↓ to move · Enter to choose · Esc to cancel"
}

// head is the start of entry i's line: the pointer on the chosen one, its
// box with multi, and its number.
func (p *picker) head(i int, sym symbols) string {
	mark := "   "
	if i == p.sel {
		mark = " " + sym.pointer + " "
	}
	box := ""
	if p.multi && i < len(p.options) {
		box = "[ ] "
		if p.ticked[i] {
			box = "[x] "
		}
	}
	return fmt.Sprintf("%s%s%d. ", mark, box, i+1)
}

// lines draws the picker for line mode, the question and details wrapped
// and every line short of the width, so the terminal wraps none: the
// redraw counts lines.
func (p *picker) lines(st style, sym symbols, width int) []string {
	room := max(1, width-1)
	fit := func(s string) string { return cutText(s, room) }
	var out []string
	add := func(paint func(string) string, text, indent string) {
		pad := []span{plain(indent)}
		for _, row := range wrap([]span{plain(text)}, room, pad, pad) {
			out = append(out, paint(fit(spansText(row))))
		}
	}
	if p.title != "" {
		add(st.gray, p.title, "")
	}
	add(st.bold, p.question, "")
	for i := range p.count() {
		head := p.head(i, sym)
		if i == len(p.options) {
			out = append(out, p.otherLine(st, head, room))
			continue
		}
		o := p.options[i]
		line := fit(head + o.Label)
		if i == p.sel {
			line = st.cyan(line)
		}
		switch {
		case o.Detail == "":
		case p.detailBelow:
			out = append(out, line)
			add(st.gray, o.Detail, strings.Repeat(" ", textWidth(head)))
			continue
		default:
			if left := room - textWidth(head+o.Label) - 2; left > 3 {
				line += "  " + st.gray(cutText(o.Detail, left))
			}
		}
		out = append(out, line)
	}
	return append(out, st.gray(fit("   "+p.hint())))
}

// otherLine draws the Other entry: its label, or what is typed into it
// with its end in sight, and a cursor while it is chosen.
func (p *picker) otherLine(st style, head string, room int) string {
	left := room - textWidth(head) - 1 // the cursor takes a column
	switch {
	case !p.onOther() && len(p.typed) == 0:
		return st.gray(cutText(head+p.other, room))
	case !p.onOther():
		return cutText(head+string(p.typed), room)
	case left < 1:
		return st.cyan(cutText(head, room))
	case len(p.typed) == 0:
		return st.cyan(head) + st.cursor() + st.gray(cutText(p.other, left))
	}
	return st.cyan(head+lastColumns(string(p.typed), left)) + st.cursor()
}

// lastColumns keeps the end of s that fits in w columns, with "…" where
// it was cut.
func lastColumns(s string, w int) string {
	if textWidth(s) <= w {
		return s
	}
	if w < 1 {
		return ""
	}
	r := []rune(s)
	i, used := len(r), 1 // the "…"
	for i > 0 && used+runeWidth(r[i-1]) <= w {
		i--
		used += runeWidth(r[i])
	}
	return "…" + string(r[i:])
}

// canPick reports whether the console can take single key presses and
// redraw lines in place, which picking with the arrow keys needs.
func (c *Console) canPick() bool {
	return c.term.Interactive && c.term.Decorate && c.openKeys != nil
}

// Choose asks a question of the setup wizard with the arrow keys, or by
// number where the input isn't a terminal (setup.Chooser).
func (c *Console) Choose(ch setup.Choice) (int, string, error) {
	if !c.canPick() {
		return setup.AskNumbered(c, ch)
	}
	kb, err := c.openKeys(inputOptions{})
	if err != nil {
		return setup.AskNumbered(c, ch)
	}
	defer kb.close()
	p := newPicker(ch.Question, ch.Options, ch.Default, ch.Other, false)
	if c.runPicker(p, kb, c.interrupts) != picked {
		return 0, "", setup.ErrCancelled
	}
	c.answered(p.question, p.summary())
	idx, typed := p.answer()
	if len(idx) == 0 {
		return -1, typed, nil
	}
	return idx[0], "", nil
}

// answered leaves the question on the screen with its answer, wrapped
// between words.
func (c *Console) answered(question, answer string) {
	width := max(1, c.term.columns()-1)
	rows := wrap([]span{plain(question)}, width, nil, nil)
	var b strings.Builder
	for i, row := range rows {
		line := spansText(row)
		b.WriteString(c.st.bold(line))
		switch {
		case i < len(rows)-1:
			b.WriteString("\n")
		case textWidth(line)+1+textWidth(answer) > width:
			b.WriteString("\n" + c.st.cyan(answer) + "\n")
		default:
			b.WriteString(" " + c.st.cyan(answer) + "\n")
		}
	}
	io.WriteString(c.out, b.String())
	c.out.Flush()
}

// runPicker draws p and takes keys until it is done, then clears it. A
// signal on stop ends it as cancelled: Ctrl+C, or the end of the turn.
func (c *Console) runPicker(p *picker, kb keyboard, stop <-chan struct{}) pickStatus {
	drawn := 0
	up := func(n int) string { // CUU with 0 would move one row
		if n <= 0 {
			return "\r"
		}
		return fmt.Sprintf("\r\x1b[%dA", n)
	}
	draw := func() {
		lines := p.lines(c.st, c.term.sym(), c.term.columns())
		var b strings.Builder
		if drawn > 0 {
			b.WriteString(up(drawn - 1))
		}
		b.WriteString(strings.Join(lines, "\x1b[K\n") + "\x1b[K\x1b[J")
		drawn = len(lines)
		io.WriteString(c.out, b.String())
		c.out.Flush()
	}
	io.WriteString(c.out, "\x1b[?25l") // the picker draws its own cursor
	defer func() {
		io.WriteString(c.out, up(drawn-1)+"\x1b[J\x1b[?25h")
		c.out.Flush()
	}()
	draw()
	for {
		select {
		case <-c.quit:
			return pickCancelled
		case <-stop:
			return pickCancelled
		case <-c.cmdDone: // Ctrl+C while a /provider command runs
			return pickCancelled
		case ev, open := <-kb.events():
			if !open {
				return pickCancelled
			}
			status := picking
			switch ev.kind {
			case evKey:
				status = p.key(ev.key)
			case evPaste:
				p.paste(ev.text)
			case evResize:
			default:
				continue
			}
			if status != picking {
				return status
			}
			draw()
		}
	}
}

// secret reads an API key with single key presses: the key never shows,
// only how long it is. ok is false where the console can't.
func (c *Console) secret(prompt string) (key string, ok bool, err error) {
	if !c.canPick() {
		return "", false, nil
	}
	kb, err := c.openKeys(inputOptions{})
	if err != nil {
		return "", false, nil
	}
	defer kb.close()
	var typed []rune
	draw := func(note string) {
		io.WriteString(c.out, "\r\x1b[2K"+c.question(prompt)+c.st.gray(note))
		c.out.Flush()
	}
	count := func() string {
		if len(typed) == 0 {
			return ""
		}
		return fmt.Sprintf("(%d characters)", len(typed))
	}
	draw("")
	for {
		select {
		case <-c.quit:
			draw("")
			io.WriteString(c.out, "\n")
			return "", true, setup.ErrCancelled
		case <-c.interrupts:
			draw("")
			io.WriteString(c.out, "\n")
			return "", true, setup.ErrCancelled
		case <-c.cmdDone:
			draw("")
			io.WriteString(c.out, "\n")
			return "", true, setup.ErrCancelled
		case ev, open := <-kb.events():
			if !open {
				return "", true, setup.ErrCancelled
			}
			switch {
			case ev.kind == evPaste:
				typed = append(typed, []rune(strings.Join(strings.Fields(ev.text), ""))...)
			case ev.kind != evKey:
			case ev.key.k == keyEnter && len(typed) > 0:
				draw("(hidden)")
				io.WriteString(c.out, "\n")
				return strings.TrimSpace(string(typed)), true, nil
			case ev.key.k == keyEsc || ev.key.ctrl('c'):
				draw("")
				io.WriteString(c.out, "\n")
				return "", true, setup.ErrCancelled
			case ev.key.k == keyBackspace && len(typed) > 0:
				typed = typed[:len(typed)-1]
			case ev.key.ctrl('u'):
				typed = nil
			case ev.key.k == keyRune && ev.key.mod&(modCtrl|modAlt) == 0 && ev.key.r > ' ':
				typed = append(typed, ev.key.r)
			}
			draw(count())
		}
	}
}
