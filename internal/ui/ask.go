package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"debbie/internal/agent"
	"debbie/internal/setup"
)

// The model's questions (the ask_user tool, agent.Asker), asked as Claude
// Code asks them: the answers picked with the keys, or one typed into the
// last entry. The fullscreen UI asks in a panel with a tab for each
// question; line mode asks them one after another.

// askOther labels the entry where the user types an answer of their own.
const askOther = "Type something else"

// askPicker makes the picker for question i of n.
func askPicker(q agent.Question, i, n int) *picker {
	var options []setup.Option
	for _, o := range q.Options {
		options = append(options, setup.Option{Label: o.Label, Detail: o.Description})
	}
	p := newPicker(q.Text, options, 0, askOther, q.Multi)
	p.detailBelow = true
	p.title = q.Header
	if n > 1 {
		p.title = strings.TrimSpace(fmt.Sprintf("%s (%d of %d)", q.Header, i+1, n))
	}
	return p
}

// askAnswer is the answer p holds.
func askAnswer(p *picker) agent.Answer {
	idx, typed := p.answer()
	a := agent.Answer{Typed: typed}
	for _, i := range idx {
		a.Picked = append(a.Picked, p.options[i].Label)
	}
	return a
}

// AskUser asks the questions one after another (agent.Asker). Esc on any
// leaves them all unanswered; Ctrl+C stops the turn.
func (c *chat) AskUser(ctx context.Context, qs []agent.Question) ([]agent.Answer, bool, error) {
	c.r.stopSpinner()
	if !c.canPick() {
		return nil, false, errors.New("this terminal can't show questions to pick answers from")
	}
	kb, err := c.openKeys(inputOptions{})
	if err != nil {
		return nil, false, err
	}
	defer kb.close()
	answers := make([]agent.Answer, len(qs))
	for i, q := range qs {
		p := askPicker(q, i, len(qs))
		if c.runPicker(p, kb, ctx.Done()) != picked {
			return nil, false, ctx.Err() // nil after Esc: not answered
		}
		answers[i] = askAnswer(p)
	}
	return answers, true, nil
}

// AskUser puts the questions in a panel and waits for the answers
// (agent.Asker).
func (f *fullscreen) AskUser(ctx context.Context, qs []agent.Question) ([]agent.Answer, bool, error) {
	reply := make(chan askReply, 1)
	f.post(func() {
		// The call shows as asking, as an approval does, rather than as
		// running: it waits for the user.
		for _, e := range f.tools {
			if b, ok := e.b.(*toolBlock); ok && b.run.Name == "ask_user" && b.state == toolRunning {
				b.state = toolAsking
				e.changed()
			}
		}
		f.openPanel(newAskPanel(qs, reply))
		f.activity = "Waiting for you"
		f.scroll = 0
	})
	select {
	case r := <-reply:
		return r.answers, r.answered, nil
	case <-ctx.Done():
		f.post(func() {
			if a, ok := f.panel.(*askPanel); ok && a.reply == reply {
				f.closePanel()
			}
		})
		return nil, false, ctx.Err()
	}
}

type askReply struct {
	answers  []agent.Answer
	answered bool
}

// askPanel asks the questions: one at a time, with a tab for each above
// it when there are several, and then a last tab to look the answers over
// and send them.
type askPanel struct {
	qs      []agent.Question
	pickers []*picker
	done    []bool // the questions answered
	at      int    // the tab shown; len(qs) is the review
	send    int    // on the review: 0 sends the answers, 1 doesn't
	reply   chan askReply
	owners  []int // the entry each row drawn shows, or -1
}

func newAskPanel(qs []agent.Question, reply chan askReply) *askPanel {
	a := &askPanel{qs: qs, done: make([]bool, len(qs)), reply: reply}
	for i, q := range qs {
		a.pickers = append(a.pickers, askPicker(q, i, len(qs)))
	}
	return a
}

func (a *askPanel) reviewing() bool { return a.at == len(a.qs) }

// answer replies to the agent, once.
func (a *askPanel) answer(answered bool) {
	if a.reply == nil {
		return
	}
	r := askReply{answered: answered}
	if answered {
		for i, p := range a.pickers {
			ans := agent.Answer{}
			if a.done[i] {
				ans = askAnswer(p)
			}
			r.answers = append(r.answers, ans)
		}
	}
	a.reply <- r
	a.reply = nil
}

func (a *askPanel) key(f *fullscreen, k keyPress) bool {
	n := len(a.qs)
	switch {
	case k.k == keyEsc:
		a.answer(false)
		return true
	case n > 1 && k.k == keyLeft && k.mod == 0:
		a.at = (a.at + n) % (n + 1)
		return false
	case n > 1 && k.k == keyRight && k.mod == 0:
		a.at = (a.at + 1) % (n + 1)
		return false
	case a.reviewing():
		switch choiceKey(k, &a.send, 2) {
		case 0:
			a.answer(true)
			return true
		case 1:
			a.answer(false)
			return true
		}
		return false
	}
	switch a.pickers[a.at].key(k) {
	case pickCancelled:
		a.answer(false)
		return true
	case picked:
		a.done[a.at] = true
		if n == 1 {
			a.answer(true)
			return true
		}
		a.next()
	}
	return false
}

// next shows the next question not answered yet, or else the review,
// where questions skipped on the way show as not answered.
func (a *askPanel) next() {
	for i := a.at + 1; i < len(a.qs); i++ {
		if !a.done[i] {
			a.at = i
			return
		}
	}
	a.at = len(a.qs)
}

func (a *askPanel) click(f *fullscreen, row int) bool {
	if row < 0 || row >= len(a.owners) || a.owners[row] < 0 {
		return false
	}
	i := a.owners[row]
	if a.reviewing() {
		a.send = i
		return a.key(f, keyPress{k: keyEnter})
	}
	p := a.pickers[a.at]
	p.sel = i
	switch {
	case p.onOther(): // typing goes there now
		return false
	case p.multi:
		p.ticked[i] = !p.ticked[i]
		return false
	}
	return a.key(f, keyPress{k: keyEnter})
}

// paste types pasted text into the answer of one's own.
func (a *askPanel) paste(f *fullscreen, text string) {
	if !a.reviewing() {
		a.pickers[a.at].paste(text)
	}
}

func (a *askPanel) rows(f *fullscreen, width int) (rows [][]span) {
	defer func() { // an indent can be wider than a narrow screen
		for i := range rows {
			rows[i] = cut(rows[i], width)
		}
	}()
	a.owners = a.owners[:0]
	add := func(owner int, rs ...[]span) {
		for _, r := range rs {
			rows = append(rows, r)
			a.owners = append(a.owners, owner)
		}
	}
	sym := f.term.sym()
	add(-1, rule(width))
	if tabs := a.tabs(sym); tabs != nil {
		add(-1, cut(tabs, width))
	}
	selected := look{fg: inkCyan, bold: true}
	if a.reviewing() {
		add(-1, []span{plain(" "), styled("Look over your answers", look{bold: true})})
		for i, q := range a.qs {
			add(-1, wrap([]span{plain(q.Text)}, width, []span{plain(" " + sym.bullet + " ")}, []span{plain("   ")})...)
			answer := inked("(not answered)", inkYellow)
			if a.done[i] {
				answer = inked(askWords(askAnswer(a.pickers[i])), inkCyan)
			}
			add(-1, wrap([]span{answer}, width, []span{grayed("   → ")}, []span{plain("     ")})...)
		}
		for i, label := range []string{"Send the answers", "Cancel"} {
			mark, l := "   ", look{}
			if i == a.send {
				mark, l = " "+sym.pointer+" ", selected
			}
			add(i, wrap([]span{styled(fmt.Sprintf("%s%d. %s", mark, i+1, label), l)}, width, nil, []span{plain("      ")})...)
		}
		add(-1, wrap([]span{grayed("   ↑↓ to move · Enter to choose · ←→ for the questions · Esc to cancel")}, width, nil, nil)...)
		return rows
	}
	p := a.pickers[a.at]
	add(-1, wrap([]span{styled(p.question, look{bold: true})}, width, []span{plain(" ")}, []span{plain(" ")})...)
	cursor := styled(" ", look{reverse: true})
	for i := range p.count() {
		head := p.head(i, sym)
		indent := []span{plain(strings.Repeat(" ", textWidth(head)))}
		l := look{}
		if i == p.sel {
			l = selected
		}
		if i == len(p.options) { // the answer of one's own
			var line []span
			switch {
			case i == p.sel && len(p.typed) == 0:
				line = []span{styled(head, l), cursor, grayed(p.other)}
			case i == p.sel:
				line = []span{styled(head, l), styled(string(p.typed), l), cursor}
			case len(p.typed) == 0:
				line = []span{grayed(head + p.other)}
			default:
				line = []span{plain(head + string(p.typed))}
			}
			add(i, wrap(line, width, nil, indent)...)
			continue
		}
		o := p.options[i]
		add(i, wrap([]span{styled(head, l), styled(o.Label, l)}, width, nil, indent)...)
		if o.Detail != "" {
			add(i, wrap([]span{grayed(o.Detail)}, width, indent, indent)...)
		}
	}
	hint := p.hint()
	if len(a.qs) > 1 {
		hint += " · ←→ for the other questions"
	}
	add(-1, wrap([]span{grayed("   " + hint)}, width, nil, nil)...)
	return rows
}

// tabs draws a tab for each question, and the review's, when there are
// several; for one, its header alone, if it has one.
func (a *askPanel) tabs(sym symbols) []span {
	if len(a.qs) == 1 && a.qs[0].Header == "" {
		return nil
	}
	tab := func(label string, i int) []span {
		if i < len(a.qs) && a.done[i] {
			label = sym.check + " " + label
		}
		l := look{fg: inkGray}
		if i == a.at {
			l = look{reverse: true, bold: true}
		}
		return []span{styled(" "+label+" ", l), plain(" ")}
	}
	out := []span{plain(" ")}
	if len(a.qs) == 1 {
		return append(out, tab(a.qs[0].Header, 0)...)
	}
	out = append(out, grayed("← "))
	for i, q := range a.qs {
		label := q.Header
		if label == "" {
			label = fmt.Sprintf("Question %d", i+1)
		}
		out = append(out, tab(label, i)...)
	}
	out = append(out, tab("Send", len(a.qs))...)
	return append(out, grayed("→"))
}

// askWords is an answer in words, for the review.
func askWords(ans agent.Answer) string {
	parts := append([]string(nil), ans.Picked...)
	if ans.Typed != "" {
		parts = append(parts, ans.Typed)
	}
	if len(parts) == 0 {
		return "(nothing)"
	}
	return strings.Join(parts, ", ")
}

// pasteTaker is a panel that takes pasted text.
type pasteTaker interface {
	paste(f *fullscreen, text string)
}
