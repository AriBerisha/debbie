package ui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"debbie/internal/agent"
	"debbie/internal/app"
	"debbie/internal/tools"
)

// The fullscreen UI's conversation: blocks of what was said and done, each
// drawn as rows for the width of the screen.

// block is one entry of the conversation.
type block interface {
	draw(width int, t Terminal) [][]span
}

// entry is a block in the transcript, with its rows as last drawn.
type entry struct {
	b     block
	width int // the width rows were drawn for; 0 after a change
	rows  [][]span
}

// changed makes the entry draw its block again.
func (e *entry) changed() { e.width = 0 }

// remove takes an entry out of the transcript.
func (tr *transcript) remove(e *entry) {
	if i := slices.Index(tr.entries, e); i >= 0 {
		tr.entries = slices.Delete(tr.entries, i, i+1)
	}
}

type transcript struct {
	entries []*entry
	t       Terminal
}

func (tr *transcript) add(b block) *entry {
	e := &entry{b: b}
	tr.entries = append(tr.entries, e)
	return e
}

// rows draws the conversation, with a blank row between blocks, and says
// which entry each row belongs to.
func (tr *transcript) rows(width int) (rows [][]span, owners []*entry) {
	for _, e := range tr.entries {
		if e.width != width {
			e.rows, e.width = e.b.draw(width, tr.t), width
		}
		if len(e.rows) == 0 {
			continue
		}
		if len(rows) > 0 {
			rows = append(rows, nil)
			owners = append(owners, nil)
		}
		rows = append(rows, e.rows...)
		for range e.rows {
			owners = append(owners, e)
		}
	}
	return rows, owners
}

// bannerBlock is the logo and, beside it, the program, the model and the
// directory, as at the top of the line-mode chat.
type bannerBlock struct {
	detail, dir string
}

func (b *bannerBlock) draw(width int, t Terminal) [][]span {
	// The logo stays while the model line fits beside it; the directory
	// keeps its end when it is too long.
	cells := logoCells()
	room := width - len(cells[0]) - len(logoGap)
	if room < max(textWidth("Code v"+app.Version), textWidth(b.detail)) {
		room = width
	}
	text := [][]span{
		{styled("Code", look{bold: true}), grayed(" v" + app.Version)},
		{grayed(clip(b.detail, room))},
		{grayed(clipLeft(homeRelative(b.dir), room))},
	}
	if room == width {
		return text
	}
	var rows [][]span
	for i, row := range cells {
		var r []span
		for x, c := range row {
			l := look{}
			if c != ' ' {
				l.fg = rgbInk(logoFrom.mix(logoTo, float64(x)/float64(max(1, len(row)-1))))
			}
			r = append(r, span{string(c), l})
		}
		if i < len(text) {
			r = append(append(r, plain(logoGap)), text[i]...)
		}
		rows = append(rows, r)
	}
	return rows
}

// noteBlock is a message from the program: a command's output, a warning,
// an error.
type noteBlock struct {
	spans []span
}

func note(text string, k ink) *noteBlock { return &noteBlock{spans: []span{inked(text, k)}} }

func (b *noteBlock) draw(width int, t Terminal) [][]span {
	return wrap(b.spans, width, []span{plain("  ")}, []span{plain("  ")})
}

// bandLook is the background of the user's messages.
var bandLook = look{bg: rgbInk(rgb{0x2c, 0x2c, 0x2c})}

// userBlock is a message the user sent, on a band across the screen, and
// what its @ mentions attached.
type userBlock struct {
	text     string
	attached []attached
}

func (b *userBlock) draw(width int, t Terminal) [][]span {
	rows := wrap([]span{styled(b.text, look{bg: bandLook.bg, fg: inkWhite})}, width-1,
		[]span{styled(" > ", look{bg: bandLook.bg, fg: inkGray})}, []span{styled("   ", bandLook)})
	for i, r := range rows {
		if pad := width - spansWidth(r); pad > 0 {
			rows[i] = append(r, styled(strings.Repeat(" ", pad), bandLook))
		}
	}
	for _, a := range b.attached {
		k := inkGray
		if !a.ok {
			k = inkYellow
		}
		rows = append(rows, wrap([]span{inked(a.text, k)}, width, []span{grayed("  " + t.sym().elbow + " ")}, []span{plain("    ")})...)
	}
	return rows
}

// replyBlock is one reply of the model: what it thought, folded once it
// answers, and its answer in Markdown.
type replyBlock struct {
	reasoning, text strings.Builder
	started         time.Time
	thought         time.Duration // how long it thought, once it answers
	done            bool
	unfold          *bool // the UI's "show thinking" switch
}

func (b *replyBlock) draw(width int, t Terminal) [][]span {
	var rows [][]span
	indent := []span{plain("  ")}
	if r := strings.TrimSpace(b.reasoning.String()); r != "" {
		answering := b.text.Len() > 0 || b.done
		header := "Thinking…"
		if answering {
			d := b.thought
			if d == 0 {
				d = time.Since(b.started)
			}
			header = fmt.Sprintf("Thought for %s", seconds(d))
		}
		hint := ""
		if answering && !*b.unfold {
			hint = " (ctrl+o to show)"
		}
		rows = append(rows, []span{styled("* "+header, look{fg: inkGray, italic: true}), grayed(hint)})
		if !answering || *b.unfold {
			lines := wrap([]span{styled(r, look{fg: inkGray, italic: true})}, width, indent, indent)
			if !answering && len(lines) > 6 {
				lines = lines[len(lines)-6:] // the latest while it thinks
			}
			rows = append(rows, lines...)
		}
	}
	if text := strings.TrimSpace(b.text.String()); text != "" {
		if len(rows) > 0 {
			rows = append(rows, nil)
		}
		md := renderMarkdown(text, width-2)
		for i, r := range md {
			prefix := indent
			if i == 0 {
				prefix = []span{styled(t.sym().bullet+" ", look{fg: inkWhite})}
			}
			rows = append(rows, append(append([]span(nil), prefix...), r...))
		}
	}
	return rows
}

// seconds writes a duration briefly: 0.4s, 12s, 3m05s.
func seconds(d time.Duration) string {
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// toolState is where a tool call is.
type toolState uint8

const (
	toolAsking toolState = iota // waiting for the user's approval
	toolRunning
	toolDone
)

// toolBlock is a tool call: its summary, what it would change or run while
// it waits for approval, and how it ended.
type toolBlock struct {
	run      agent.ToolRun // a copy: the agent's goroutine owns the original
	state    toolState
	started  time.Time
	answer   string               // what the user said when asked
	expanded bool                 // all of its output is shown (a click)
	details  *bool                // the UI's "show details" switch (ctrl+o), for every block
	sub      *agent.SubagentState // a task call's subagent, as last heard of
}

// foldedSubagentCalls is how many of a subagent's calls show, the last
// ones, until its task call is unfolded.
const foldedSubagentCalls = 4

// Output a tool block shows: a command's first lines, folded; the rest, up
// to a limit, when the block is unfolded.
const (
	foldedOutputLines   = 3
	unfoldedOutputLines = 300
)

// outputLines are the lines of a call's output worth showing: without the
// untrusted-content tags around an MCP result and the exit code line the
// shell adds, which the result line already shows.
func (b *toolBlock) outputLines() []string {
	out := strings.TrimRight(strings.ReplaceAll(b.run.Output, "\r\n", "\n"), "\n ")
	if b.run.Status != agent.RunOK || out == "" {
		return nil
	}
	lines := strings.Split(out, "\n")
	if strings.HasPrefix(lines[0], "<untrusted_mcp_result ") && lines[len(lines)-1] == "</untrusted_mcp_result>" {
		lines = lines[1 : len(lines)-1]
	}
	if n := len(lines); n > 0 && strings.HasPrefix(lines[n-1], "Exit code: ") && b.run.Name == "shell" {
		lines = lines[:n-1]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// unfolded reports whether all of the block's output is shown.
func (b *toolBlock) unfolded() bool { return b.expanded || b.details != nil && *b.details }

// subagentRows are a task call's subagent's calls, a line each: the last
// few, or all of them unfolded.
func (b *toolBlock) subagentRows(width int) [][]span {
	if b.sub == nil {
		return nil
	}
	calls := b.sub.Calls
	var rows [][]span
	if hidden := len(calls) - foldedSubagentCalls; hidden > 0 && !b.unfolded() {
		rows = append(rows, []span{plain("    "), grayed("… " + tools.Count(hidden, "earlier call", "earlier calls") + " (click or ctrl+o to show)")})
		calls = calls[hidden:]
	}
	indent := []span{plain("    ")}
	for _, c := range calls {
		line := []span{grayed(c.Summary)}
		switch {
		case c.Running:
			line = append(line, grayed(" …"))
		case c.Status == agent.RunFailed:
			line = append(line, inked(" · error: "+firstLine(c.Error, 120), inkRed))
		case c.Status == agent.RunDenied:
			line = append(line, inked(" · declined", inkYellow))
		case c.Status == agent.RunCancelled:
			line = append(line, inked(" · cancelled", inkYellow))
		case c.Display != "":
			line = append(line, grayed(" · "+c.Display))
		}
		rows = append(rows, wrap(line, width, indent, []span{plain("      ")})...)
	}
	if b.sub.Done && b.sub.Err != "" {
		rows = append(rows, wrap([]span{inked("the subagent stopped: "+firstLine(b.sub.Err, 200), inkRed)}, width, indent, indent)...)
	}
	return rows
}

// foldable reports whether a click would show more, or less, of its output.
func (b *toolBlock) foldable() bool {
	if b.sub != nil && len(b.sub.Calls) > foldedSubagentCalls {
		return true
	}
	n := len(b.outputLines())
	if b.run.Kind != tools.Execute {
		return n > 0
	}
	return n > foldedOutputLines
}

func (b *toolBlock) draw(width int, t Terminal) [][]span {
	sym := t.sym()
	bullet := look{fg: inkGray}
	if b.state == toolDone {
		switch b.run.Status {
		case agent.RunOK:
			bullet.fg = inkGreen
			if b.run.Warn {
				bullet.fg = inkYellow
			}
		case agent.RunFailed:
			bullet.fg = inkRed
		default:
			bullet.fg = inkYellow
		}
	}
	name, rest, _ := strings.Cut(b.run.Summary, " ")
	head := []span{styled(name, look{bold: true})}
	if rest != "" {
		head = append(head, plain(" "+rest))
	}
	rows := wrap(head, width, []span{styled(sym.bullet+" ", bullet)}, []span{plain("  ")})

	// The change or command, while asking and after an edit nobody saw.
	showDiff := b.run.Kind == tools.Edit && b.run.Preview != "" && (b.state == toolAsking || b.state == toolDone && !b.run.Asked && b.run.Status == agent.RunOK)
	switch {
	case showDiff:
		limit := 60
		if b.state == toolAsking {
			limit = 200
		}
		rows = append(rows, diffRows(b.run.Preview, width, limit)...)
	case b.state == toolAsking && b.run.Preview != "" && (strings.Contains(b.run.Preview, "\n") || !strings.HasSuffix(b.run.Summary, b.run.Preview)):
		for _, l := range strings.Split(b.run.Preview, "\n") {
			rows = append(rows, wrap([]span{plain(l)}, width, []span{plain("    ")}, []span{plain("    ")})...)
		}
	}
	if b.state == toolAsking {
		for _, p := range b.run.Outside {
			rows = append(rows, wrap([]span{inked("outside the workspace: "+p, inkYellow)}, width, []span{plain("    ")}, []span{plain("    ")})...)
		}
	}

	var result []span
	switch {
	case b.sub != nil && !b.sub.Done:
		if b.sub.Waiting {
			result = []span{grayed("waiting for a free slot")}
		} else {
			result = []span{inked("running", inkCyan), grayed(" · " + tools.Count(len(b.sub.Calls), "call", "calls") + " · " + seconds(time.Since(b.sub.Started)))}
		}
	case b.state == toolRunning:
		result = []span{grayed("running… " + seconds(time.Since(b.started)))}
	case b.state != toolDone:
	case b.run.Status == agent.RunOK:
		text := b.run.Display
		if b.sub != nil {
			text = tools.Count(len(b.sub.Calls), "call", "calls")
		}
		if text == "" {
			text = "done"
		}
		if b.run.Elapsed >= 2*time.Second {
			text += " · " + seconds(b.run.Elapsed)
		}
		k := inkGray
		if b.run.Warn {
			k = inkYellow
		}
		result = []span{inked(text, k)}
	case b.run.Status == agent.RunFailed:
		result = []span{inked("error: "+firstLine(b.run.Error, 300), inkRed)}
	case b.run.Status == agent.RunDenied:
		result = []span{inked("declined", inkYellow)}
	case b.run.Status == agent.RunCancelled:
		result = []span{inked("cancelled", inkYellow)}
	}
	if b.answer != "" && b.state == toolDone && b.run.Status == agent.RunOK {
		result = append(result, grayed(" · "+b.answer))
	}
	if len(result) > 0 {
		rows = append(rows, wrap(result, width, []span{grayed("  " + sym.elbow + " ")}, []span{plain("    ")})...)
	}
	rows = append(rows, b.subagentRows(width)...)

	// The output: a command's first lines, everything when unfolded.
	if b.state == toolDone {
		lines := b.outputLines()
		show := 0
		switch {
		case b.unfolded():
			show = min(len(lines), unfoldedOutputLines)
		case b.run.Kind == tools.Execute:
			show = min(len(lines), foldedOutputLines)
		}
		indent := []span{plain("    ")}
		for _, l := range lines[:show] {
			rows = append(rows, wrap([]span{grayed(clean(l))}, width, indent, indent)...)
		}
		switch hidden := len(lines) - show; {
		case hidden > 0 && !b.unfolded() && b.sub != nil:
			rows = append(rows, []span{plain("    "), grayed(fmt.Sprintf("… its report, %s (ctrl+o to show) · click to open the subagent", tools.Lines(hidden)))})
		case hidden > 0 && !b.unfolded():
			rows = append(rows, []span{plain("    "), grayed(fmt.Sprintf("… +%s (click or ctrl+o to show)", tools.Lines(hidden)))})
		case hidden > 0:
			rows = append(rows, []span{plain("    "), grayed(fmt.Sprintf("… %d more lines", hidden))})
		}
	}
	return rows
}

// diffRows draws a unified diff in color, indented, up to limit lines.
func diffRows(diff string, width, limit int) [][]span {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	var rows [][]span
	indent := []span{plain("    ")}
	for i, l := range lines {
		if i == limit {
			rows = append(rows, []span{plain("    "), grayed(fmt.Sprintf("… %d more lines", len(lines)-limit))})
			break
		}
		k := ink{}
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
			k = inkGray
		case strings.HasPrefix(l, "@@"):
			k = inkCyan
		case strings.HasPrefix(l, "+"):
			k = inkGreen
		case strings.HasPrefix(l, "-"):
			k = inkRed
		}
		rows = append(rows, wrap([]span{inked(l, k)}, width, indent, indent)...)
	}
	return rows
}

// dividerBlock is a line across the conversation with a label, such as
// where it was compacted.
type dividerBlock struct {
	text string
}

func (b *dividerBlock) draw(width int, t Terminal) [][]span {
	label := " " + b.text + " "
	side := max(2, (width-textWidth(label))/2)
	return [][]span{cut([]span{grayed(strings.Repeat("─", side) + label + strings.Repeat("─", side))}, width)}
}
