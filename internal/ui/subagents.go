package ui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"debbie/internal/agent"
	"debbie/internal/config"
	"debbie/internal/provider"
	"debbie/internal/tools"
)

// convo is a conversation on screen: the main one, or a subagent's.
type convo struct {
	tr    transcript
	reply *entry // the reply being streamed
	// replyIn redacts the reply's text and reasoning as they stream in.
	replyIn struct{ text, reasoning *config.Pieces }
	tools   map[*agent.ToolRun]*entry // tool calls by the agent's ToolRun, used as a key only
}

func (c *convo) replyStart(f *fullscreen) {
	c.reply = c.tr.add(&replyBlock{started: time.Now(), unfold: &f.showThinking})
	c.replyIn.text, c.replyIn.reasoning = f.opts.Redactor.Pieces(), f.opts.Redactor.Pieces()
}

// event adds a streamed piece to the reply. ok says whether it was one,
// and text whether it was text rather than reasoning.
func (c *convo) event(ev provider.Event) (text, ok bool) {
	if c.reply == nil {
		return false, false
	}
	b := c.reply.b.(*replyBlock)
	switch e := ev.(type) {
	case provider.TextDelta:
		if b.text.Len() == 0 && b.reasoning.Len() > 0 {
			b.thought = time.Since(b.started)
		}
		b.text.WriteString(c.replyIn.text.Add(e.Text))
		text = true
	case provider.ReasoningDelta:
		b.reasoning.WriteString(c.replyIn.reasoning.Add(e.Text))
	case provider.Note:
		// A note comes whole: nothing of it waits for the next piece.
		in := c.replyIn.reasoning
		b.notes = append(b.notes, strings.TrimSpace(in.Add(e.Text)+in.End()))
	default:
		return false, false
	}
	c.reply.changed()
	return text, true
}

func (c *convo) replyEnd() {
	if c.reply != nil {
		b := c.reply.b.(*replyBlock)
		b.text.WriteString(c.replyIn.text.End())
		b.reasoning.WriteString(c.replyIn.reasoning.End())
		b.done = true
		if b.thought == 0 && b.reasoning.Len() > 0 {
			b.thought = time.Since(b.started)
		}
		c.reply.changed()
	}
	c.reply = nil
}

// entryFor returns the block of a tool call, adding it the first time.
func (c *convo) entryFor(run *agent.ToolRun, snap agent.ToolRun, details *bool) (*entry, *toolBlock) {
	e, ok := c.tools[run]
	if !ok {
		e = c.tr.add(&toolBlock{run: snap, state: toolRunning, started: time.Now(), details: details})
		c.tools[run] = e
	}
	b := e.b.(*toolBlock)
	b.run = snap
	e.changed()
	return e, b
}

// subagentView is a subagent's own conversation, which the user opens to
// watch it and talk to it. It is the subagent's display host.
type subagentView struct {
	f *fullscreen
	convo
	state agent.SubagentState // as last heard
	task  *entry              // its task call in the main conversation
}

func (v *subagentView) ReplyStart() { v.f.post(func() { v.replyStart(v.f) }) }
func (v *subagentView) ReplyEnd()   { v.f.post(v.replyEnd) }

func (v *subagentView) Event(ev provider.Event) { v.f.post(func() { v.event(ev) }) }

func (v *subagentView) ToolStart(run *agent.ToolRun) {
	snap := *run
	v.f.post(func() {
		_, b := v.entryFor(run, snap, &v.f.showThinking)
		b.state, b.started = toolRunning, time.Now()
	})
}

func (v *subagentView) ToolEnd(run *agent.ToolRun) {
	snap := *run
	v.f.post(func() {
		_, b := v.entryFor(run, snap, &v.f.showThinking)
		b.state = toolDone
		delete(v.tools, run)
	})
}

// Approve isn't used: a subagent's approvals go to the main host.
func (v *subagentView) Approve(context.Context, *agent.ToolRun) (agent.Approval, error) {
	return agent.Approval{}, errors.New("a subagent's view doesn't approve")
}

// SubagentHost makes the view of a subagent that starts: its task first,
// as the message it got.
func (f *fullscreen) SubagentHost(s agent.SubagentState) agent.Host {
	v := &subagentView{f: f, state: s}
	v.tr.t = f.term
	v.tools = map[*agent.ToolRun]*entry{}
	v.tr.add(&userBlock{text: s.Prompt})
	f.post(func() { f.views[s.ID] = v })
	return v
}

// Subagent shows a subagent's progress: under its task call, in its view's
// header and in the strip under the input box.
func (f *fullscreen) Subagent(task *agent.ToolRun, s agent.SubagentState) {
	f.post(func() {
		v := f.views[s.ID]
		if v != nil {
			v.state = s
			if v.task == nil {
				v.task = f.tools[task]
			}
		}
		e := f.tools[task]
		if v != nil && v.task != nil {
			e = v.task // also once the task call ended, for a later turn
		}
		if e != nil {
			e.b.(*toolBlock).sub = &s
			e.changed()
		}
	})
}

// subagentList returns the subagents for the strip: with all, every one
// of the session, else those at work. Those working come first, the
// longest working first; then those waiting; then those done, the latest
// first.
func (f *fullscreen) subagentList(all bool) []*subagentView {
	var list []*subagentView
	for _, v := range f.views {
		if all || !v.state.Done {
			list = append(list, v)
		}
	}
	rank := func(s agent.SubagentState) int {
		switch {
		case s.Done:
			return 2
		case s.Waiting:
			return 1
		}
		return 0
	}
	slices.SortFunc(list, func(a, b *subagentView) int {
		if ra, rb := rank(a.state), rank(b.state); ra != rb {
			return ra - rb
		}
		if a.state.Done {
			return b.state.ID - a.state.ID
		}
		if c := a.state.Started.Compare(b.state.Started); c != 0 {
			return c
		}
		return a.state.ID - b.state.ID
	})
	return list
}

// stripItem is where a subagent is on the strip, for clicks.
type stripItem struct {
	from, to int // columns
	v        *subagentView
}

// subagentRow is the strip of subagents under the input box, like Claude
// Code's background tasks: those at work side by side; focused (↓ from an
// empty input box), all of the session, one of them chosen.
func (f *fullscreen) subagentRow(width int) []span {
	f.stripItems = f.stripItems[:0]
	focused := f.stripFocus >= 0
	list := f.subagentList(focused)
	if len(list) == 0 {
		f.stripFocus = -1
		return nil
	}
	if focused {
		f.stripFocus = min(f.stripFocus, len(list)-1)
	}
	sym := f.term.sym()
	running, waiting := 0, 0
	for _, v := range list {
		switch {
		case v.state.Waiting:
			waiting++
		case !v.state.Done:
			running++
		}
	}
	label := tools.Count(running, "subagent", "subagents") + " running"
	if waiting > 0 {
		label += fmt.Sprintf(", %d waiting", waiting)
	}
	if focused {
		label = "subagents"
	}
	row := []span{plain("  "), bolded(label, ink{})}
	for i, v := range list {
		s := v.state
		mark, k, about := sym.bullet, inkCyan, seconds(time.Since(s.Started))
		switch {
		case s.Waiting:
			mark, k, about = "○", inkGray, "waiting"
		case s.Done && s.Err != "":
			mark, k, about = sym.cross, inkRed, "stopped"
		case s.Done:
			mark, k, about = sym.check, inkGreen, tools.Count(len(s.Calls), "call", "calls")
		}
		item := []span{inked(mark+" ", k), plain(clip(s.Description, 30)), grayed(" " + about)}
		if focused && i == f.stripFocus {
			for j := range item {
				item[j].look.reverse = true
			}
		}
		more := []span{grayed(fmt.Sprintf("   +%d more", len(list)-i))}
		if spansWidth(row)+3+spansWidth(item)+spansWidth(more) > width && i < len(list)-1 && !(focused && i <= f.stripFocus) {
			row = append(row, more...)
			break
		}
		row = append(row, plain("   "))
		from := spansWidth(row)
		row = append(row, item...)
		f.stripItems = append(f.stripItems, stripItem{from, spansWidth(row), v})
	}
	return row
}

// clickStrip opens the subagent clicked on the strip.
func (f *fullscreen) clickStrip(x int) {
	for _, it := range f.stripItems {
		if x >= it.from && x < it.to {
			f.openView(it.v)
			return
		}
	}
}

// stripKey handles a key while the strip is focused, reporting whether it
// took it.
func (f *fullscreen) stripKey(p keyPress) bool {
	if f.stripFocus < 0 {
		return false
	}
	list := f.subagentList(true)
	switch {
	case len(list) == 0:
		f.stripFocus = -1
		return false
	case p.k == keyLeft:
		f.stripFocus = max(0, f.stripFocus-1)
	case p.k == keyRight:
		f.stripFocus = min(len(list)-1, f.stripFocus+1)
	case p.k == keyEnter:
		f.openView(list[min(f.stripFocus, len(list)-1)])
	case p.k == keyDelete:
		if v := list[min(f.stripFocus, len(list)-1)]; !v.state.Done && v.state.Agent != nil {
			v.state.Agent.Stop()
			f.flash("stopping " + v.state.Description)
		}
	case p.k == keyEsc, p.k == keyUp:
		f.stripFocus = -1
	default:
		f.stripFocus = -1
		return false // the key does what it does in the input box
	}
	return true
}

// openView shows a subagent's conversation, which the input box now
// talks to.
func (f *fullscreen) openView(v *subagentView) {
	f.viewing, f.stripFocus, f.sel = v, -1, selection{}
	f.scroll, f.viewRows = 0, 0
	f.completion = -1
}

// closeView goes back to the main conversation.
func (f *fullscreen) closeView() {
	f.viewing, f.sel = nil, selection{}
	f.scroll, f.viewRows = 0, 0
}

// viewHeader is the top row of a subagent's view: the way back, and which
// subagent it is and how it is doing.
func (f *fullscreen) viewHeader(width int) frameRow {
	s := f.viewing.state
	kind := "read-only"
	if s.Full {
		kind = "full"
	}
	status := "running " + seconds(time.Since(s.Started)) + " · " + tools.Count(len(s.Calls), "call", "calls")
	switch {
	case s.Waiting:
		status = "waiting for a free slot"
	case s.Done && s.Err != "":
		status = "stopped: " + firstLine(s.Err, 80)
	case s.Done:
		status = "done · " + tools.Count(len(s.Calls), "call", "calls")
	}
	bg := rgbInk(rgb{0x1e, 0x3a, 0x3a})
	band := look{bg: bg}
	spans := []span{
		styled(" ← back ", look{fg: inkWhite, bg: bg, bold: true}),
		styled("   subagent ", look{fg: inkGray, bg: bg}),
		styled(s.Description, look{fg: rgbInk(tealBright), bg: bg, bold: true}),
		styled("  ·  "+kind+" · "+status, look{fg: inkGray, bg: bg}),
	}
	return frameRow{spans: cut(spans, width), fill: band}
}

// tellSubagent sends what the user typed to the subagent on screen.
func (f *fullscreen) tellSubagent(v *subagentView, text string) {
	note := "it gets this with its next step"
	switch {
	case v.state.Waiting:
		note = "it gets this with its task"
	case v.state.Done:
		note = "it answers in a new turn"
	}
	v.tr.add(&userBlock{text: text, attached: []attached{{text: note, ok: true}}})
	if v.state.Agent != nil {
		v.state.Agent.Tell(text)
	}
}

// forgetSubagents drops the subagents' views, as /clear does.
func (f *fullscreen) forgetSubagents() {
	for _, v := range f.views {
		if v.state.Agent != nil && !v.state.Done {
			v.state.Agent.Stop()
		}
	}
	f.views, f.viewing, f.stripFocus = map[int]*subagentView{}, nil, -1
}

// activeSubagents counts the subagents at work.
func (f *fullscreen) activeSubagents() int {
	n := 0
	for v := range maps.Values(f.views) {
		if !v.state.Done {
			n++
		}
	}
	return n
}
