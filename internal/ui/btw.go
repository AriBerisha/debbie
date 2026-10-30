package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"debbie/internal/agent"
	"debbie/internal/provider"
)

// /btw asks a side question, like Claude Code's: the model answers from the
// conversation without tools, and neither the question nor the answer joins
// the conversation, so it can be asked while a turn runs. The fullscreen UI
// shows the answer in a bubble over the input box; /btw alone opens the
// bubble on the list of side questions asked, to read their answers again.

// btwExchange is a side question and its answer.
type btwExchange struct {
	question  string
	answer    string
	asked     time.Time
	answering bool
	stopped   bool   // interrupted before the answer was complete
	err       string // why there is no answer
}

// btwInk colors the side-question bubble.
var btwInk = rgbInk(rgb{0xa9, 0x9b, 0xf2})

// earlierAsides are the side exchanges a new side question carries: the
// answered ones, oldest first.
func earlierAsides(xs []*btwExchange) []agent.Aside {
	var out []agent.Aside
	for _, x := range xs {
		if !x.answering && x.err == "" && x.answer != "" {
			out = append(out, agent.Aside{Question: x.question, Answer: x.answer})
		}
	}
	return out
}

// keepAsides drops the oldest side exchanges past the limit.
func keepAsides(xs []*btwExchange) []*btwExchange {
	if len(xs) > agent.MaxAsides {
		xs = xs[len(xs)-agent.MaxAsides:]
	}
	return xs
}

// age says how long ago t was, briefly.
func age(t time.Time) string {
	switch d := time.Since(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("Jan 2")
	}
}

// The fullscreen UI.

func (f *fullscreen) btwCommand(arg string) {
	if arg == "" {
		if len(f.btws) == 0 {
			f.say("No side questions yet. /btw <question> asks one; the answer stays out of the conversation.", inkGray)
			return
		}
		f.openPanel(&btwPanel{list: true, sel: len(f.btws) - 1})
		return
	}
	f.ask(arg)
	f.openPanel(&btwPanel{sel: len(f.btws) - 1})
}

// ask sends a side question on another goroutine. A new one stops the one
// still being answered.
func (f *fullscreen) ask(question string) {
	if f.btwCancel != nil {
		f.btwCancel()
	}
	earlier := earlierAsides(f.btws)
	x := &btwExchange{question: question, asked: time.Now(), answering: true}
	f.btws = keepAsides(append(f.btws, x))
	ctx, cancel := context.WithCancel(context.Background())
	f.btwCancel = cancel
	in := f.opts.Redactor.Pieces()
	go func() {
		answer, err := f.opts.Session.Ask(ctx, question, earlier, func(t string) {
			f.post(func() { x.answer += in.Add(t) })
		})
		f.post(func() {
			x.answering = false
			switch {
			case errors.Is(err, context.Canceled):
				x.stopped = true
			case err != nil:
				x.err = f.redact(errorText(err))
			default:
				x.answer = f.redact(answer)
			}
			cancel()
			if f.btwAsking() == nil {
				f.btwCancel = nil
			}
			if _, open := f.panel.(*btwPanel); !open && err == nil {
				f.flash("the side question is answered · /btw to read it")
			}
		})
	}()
}

// btwAsking returns the side question being answered, if any.
func (f *fullscreen) btwAsking() *btwExchange {
	for _, x := range f.btws {
		if x.answering {
			return x
		}
	}
	return nil
}

// btwPanel is the bubble: one answer, scrolled with ↑ ↓, or the list of
// side questions, which opens one with Enter or a click.
type btwPanel struct {
	list    bool // showing the list rather than an answer
	sel     int  // the exchange shown, or chosen in the list
	scroll  int  // the answer's first row shown
	listTop int  // the list's first row shown, kept while an answer is open
	visible int  // content rows shown at the last draw
	total   int  // content rows there were
	lastEnd int  // where scroll showed the end at the last draw
}

func (b *btwPanel) exchange(f *fullscreen) *btwExchange {
	if len(f.btws) == 0 {
		return nil
	}
	b.sel = max(0, min(b.sel, len(f.btws)-1))
	return f.btws[b.sel]
}

func (b *btwPanel) rows(f *fullscreen, width int) [][]span {
	inner := max(10, width-4)
	var body [][]span
	var title, where, hints string
	if b.list {
		body = b.listRows(f, inner)
		title, where = "btw · side questions", fmt.Sprintf("%d asked", len(f.btws))
		hints = "↑↓ choose · enter open · x delete · esc close"
	} else {
		body = b.answerRows(f, inner)
		title = "btw"
		if len(f.btws) > 1 {
			where = fmt.Sprintf("%d of %d", b.sel+1, len(f.btws))
			hints = "↑↓ scroll · ←→ other answers · esc list"
		} else {
			hints = "↑↓ scroll · esc close"
		}
	}

	// The bubble takes at most two thirds of the screen, and what the
	// layout leaves for panels.
	maxBody := max(1, min(f.panelLimit, max(6, f.height*2/3))-2)
	b.visible, b.total = min(len(body), maxBody), len(body)
	first := &b.scroll
	if b.list {
		first = &b.listTop
		if b.sel < b.listTop {
			b.listTop = b.sel
		}
		if b.sel >= b.listTop+b.visible {
			b.listTop = b.sel - b.visible + 1
		}
	} else if x := b.exchange(f); x != nil && x.answering && b.scroll >= b.lastEnd {
		b.scroll = b.total // a streaming answer keeps its end in sight
	}
	*first = max(0, min(*first, b.total-b.visible))
	if !b.list {
		b.lastEnd = b.total - b.visible
		if b.scroll < b.total-b.visible {
			hints += fmt.Sprintf(" · %d more ↓", b.total-b.visible-b.scroll)
		}
	}

	c := f.term.sym().corners
	border := look{fg: btwInk}
	top := []span{styled(c[0]+"─ ", border), styled(title, look{fg: btwInk, bold: true}), plain(" ")}
	if where != "" {
		top = append(top, styled(strings.Repeat("─", max(1, width-textWidth(title)-textWidth(where)-8)), border), plain(" "), grayed(where), plain(" "), styled("─"+c[1], border))
	} else {
		top = append(top, styled(strings.Repeat("─", max(1, width-textWidth(title)-5))+c[1], border))
	}
	rows := [][]span{cut(top, width)}
	for _, r := range body[*first : *first+b.visible] {
		r = cut(r, inner)
		pad := plain(strings.Repeat(" ", max(0, inner-spansWidth(r))))
		rows = append(rows, append(append([]span{styled("│ ", border)}, r...), pad, styled(" │", border)))
	}
	hints = clip(hints, max(1, width-6))
	bottom := []span{styled(c[2]+"─ ", border), grayed(hints), plain(" "), styled(strings.Repeat("─", max(1, width-textWidth(hints)-5))+c[3], border)}
	return append(rows, cut(bottom, width))
}

// answerRows are the question and its answer.
func (b *btwPanel) answerRows(f *fullscreen, width int) [][]span {
	x := b.exchange(f)
	if x == nil {
		return [][]span{{grayed("No side questions.")}}
	}
	rows := wrap([]span{bolded(f.term.sym().pointer+" ", btwInk), styled(x.question, look{bold: true})}, width, nil, []span{plain("  ")})
	rows = append(rows, nil)
	if x.answer != "" {
		rows = append(rows, renderMarkdown(x.answer, width)...)
	}
	switch {
	case x.answering:
		label := "Answering…"
		if x.answer != "" {
			rows = append(rows, nil)
			label = "Still answering…"
		}
		rows = append(rows, []span{styled(spinnerFrames[f.frame%len(spinnerFrames)]+" ", look{fg: btwInk, bold: true}), grayed(label + " (esc closes; it carries on)")})
	case x.stopped:
		rows = append(rows, []span{inked("Stopped before the answer was complete.", inkYellow)})
	case x.err != "":
		rows = append(rows, wrap([]span{inked("error: "+x.err, inkRed)}, width, nil, nil)...)
	}
	return rows
}

// listRows are the side questions, oldest first, the chosen one marked.
func (b *btwPanel) listRows(f *fullscreen, width int) [][]span {
	var rows [][]span
	for i, x := range f.btws {
		when := age(x.asked)
		if x.answering {
			when = "answering…"
		}
		q := clip(strings.ReplaceAll(x.question, "\n", " "), max(5, width-textWidth(when)-6))
		if i == b.sel {
			rows = append(rows, []span{styled(f.term.sym().pointer+" ", look{fg: btwInk, bold: true}), styled(q, look{bold: true}), grayed(" · " + when)})
		} else {
			rows = append(rows, []span{plain("  "), plain(q), grayed(" · " + when)})
		}
	}
	return rows
}

func (b *btwPanel) key(f *fullscreen, p keyPress) bool {
	if b.list {
		return b.listKey(f, p)
	}
	page := max(1, b.visible-1)
	switch {
	case p.k == keyUp, p.k == keyRune && p.r == 'k' && p.mod == 0:
		b.scroll--
	case p.k == keyDown, p.k == keyRune && p.r == 'j' && p.mod == 0:
		b.scroll++
	case p.k == keyPgUp:
		b.scroll -= page
	case p.k == keyPgDn:
		b.scroll += page
	case p.k == keyHome:
		b.scroll = 0
	case p.k == keyEnd:
		b.scroll = b.total
	case p.k == keyLeft, p.k == keyRune && p.r == '[':
		if b.sel > 0 {
			b.sel, b.scroll = b.sel-1, 0
		}
	case p.k == keyRight, p.k == keyRune && p.r == ']':
		if b.sel < len(f.btws)-1 {
			b.sel, b.scroll = b.sel+1, 0
		}
	case p.k == keyEsc, p.k == keyBackspace:
		if len(f.btws) > 1 {
			b.list = true
			return false
		}
		return true
	case p.k == keyEnter, p.k == keyRune && p.r == ' ' && p.mod == 0, p.k == keyTab:
		return true
	case p.k == keyRune && p.mod&(modCtrl|modAlt) == 0:
		f.ed.insert(string(p.r)) // typing closes the bubble
		return true
	}
	b.scroll = max(0, b.scroll)
	return false
}

func (b *btwPanel) listKey(f *fullscreen, p keyPress) bool {
	page := max(1, b.visible-1)
	switch {
	case p.k == keyUp, p.k == keyRune && p.r == 'k' && p.mod == 0:
		b.sel--
	case p.k == keyDown, p.k == keyRune && p.r == 'j' && p.mod == 0:
		b.sel++
	case p.k == keyPgUp:
		b.sel -= page
	case p.k == keyPgDn:
		b.sel += page
	case p.k == keyHome:
		b.sel = 0
	case p.k == keyEnd:
		b.sel = len(f.btws) - 1
	case p.k == keyEnter, p.k == keyRight, p.k == keyRune && p.r == ' ' && p.mod == 0:
		b.list, b.scroll = false, 0
	case p.k == keyRune && p.r == 'x' && p.mod == 0, p.k == keyDelete:
		if x := b.exchange(f); x != nil {
			if x.answering && f.btwCancel != nil {
				f.btwCancel()
			}
			f.btws = append(f.btws[:b.sel:b.sel], f.btws[b.sel+1:]...)
		}
		if len(f.btws) == 0 {
			return true
		}
	case p.k == keyEsc, p.k == keyTab:
		return true
	case p.k == keyRune && p.mod&(modCtrl|modAlt) == 0:
		f.ed.insert(string(p.r))
		return true
	}
	b.sel = max(0, min(b.sel, len(f.btws)-1))
	return false
}

// click opens the side question clicked in the list.
func (b *btwPanel) click(f *fullscreen, row int) bool {
	if !b.list {
		return false
	}
	if i := b.listTop + row - 1; row >= 1 && row <= b.visible && i < len(f.btws) {
		b.sel, b.list, b.scroll = i, false, 0
	}
	return false
}

// wheel scrolls the answer, or moves through the list.
func (b *btwPanel) wheel(f *fullscreen, up bool) {
	n := 3
	if up {
		n = -3
	}
	if b.list {
		b.sel = max(0, min(b.sel+n/3, len(f.btws)-1))
		return
	}
	b.scroll = max(0, b.scroll+n)
}

// The line-mode /btw.

// btwCommand asks a side question in line mode, showing the answer as it
// streams; /btw alone lists the side questions asked.
func (c *chat) btwCommand(arg string) {
	if arg == "" {
		if len(c.btws) == 0 {
			c.r.println(c.st.gray("No side questions yet. /btw <question> asks one; the answer stays out of the conversation."))
			return
		}
		for _, x := range c.btws {
			first, _, _ := strings.Cut(strings.TrimSpace(x.answer), "\n")
			c.r.println(c.st.bold("btw "+x.question) + c.st.gray(" · "+age(x.asked)))
			if first != "" {
				c.r.println(c.st.gray("  " + clip(first, max(10, c.term.columns()-4))))
			}
		}
		return
	}
	earlier := earlierAsides(c.btws)
	x := &btwExchange{question: arg, asked: time.Now()}
	c.btws = keepAsides(append(c.btws, x))
	ctx, stop := c.Interruptible()
	defer stop()
	c.r.println(c.st.gray("btw · a side question; the answer stays out of the conversation"))
	c.ReplyStart()
	answer, err := c.opts.Session.Ask(ctx, arg, earlier, func(t string) { c.Event(provider.TextDelta{Text: t}) })
	c.ReplyEnd()
	switch {
	case errors.Is(err, context.Canceled):
		x.stopped = true
		c.r.println(c.st.yellow("Stopped."))
	case err != nil:
		x.err = err.Error()
		c.r.println(c.st.red("error: " + errorText(err)))
	default:
		x.answer = answer
	}
}
