package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"debbie/internal/agent"
	"debbie/internal/config"
	"debbie/internal/effort"
	"debbie/internal/mcp"
	"debbie/internal/provider"
	"debbie/internal/setup"
	"debbie/internal/tools"
)

// fullscreen is the chat in the fullscreen UI (SPEC §14, phase B), like
// Claude Code's: the program draws the whole screen on the terminal's
// alternate screen, the conversation scrolls inside it, and the input box
// and status line stay at the bottom. One goroutine owns all of its state:
// input events arrive on the keyboard's channel, and the agent, which runs
// a turn on another goroutine, posts what happens as functions to run.
type fullscreen struct {
	*chat
	kb    keyboard
	scr   screen
	posts chan func()
	convo // the main conversation
	ed    *editor
	size  func() (cols, rows int)

	width, height int
	scroll        int      // conversation rows scrolled back; 0 follows the end
	viewRows      int      // rows the conversation had at the last frame
	owners        []*entry // the entry of each conversation row on screen
	inputTop      int      // the screen row of the input box's first text row
	inputShown    int      // the input box's text rows on screen
	inputScroll   int      // input rows scrolled out of a tall input box
	panelTop      int      // the screen row of the panel's first row
	panelRows     int
	panelCut      int // the panel's rows cut off at its top, too tall for the screen
	panelLimit    int // the most rows a panel may take

	// chromeShown: the status line last said Chrome is connected.
	chromeShown bool

	busy       bool
	cancel     context.CancelFunc
	turnStart  time.Time
	firstDelta time.Time
	deltas     int
	activity   string   // what the turn is doing, for the activity line
	queue      []string // messages typed while a turn ran

	// The subagents (subagents.go): each one's view by ID, the one open,
	// and the strip under the input box: the subagent chosen while ↓
	// focused it (-1 when not), where each sits, and the strip's row.
	views      map[int]*subagentView
	viewing    *subagentView
	stripFocus int
	stripItems []stripItem
	stripTop   int

	panel        panel              // a dialog above the input box; nil for none
	under        panel              // the panel an approval covers, shown again after it
	completion   int                // the highlighted command completion; -1 for none
	btwCancel    context.CancelFunc // stops the side question being answered
	frame        int
	hint         string // a passing message in the status line
	hintUntil    time.Time
	lastCtrlC    time.Time
	showThinking bool  // ctrl+o: thinking and tool output unfolded
	shown        frame // the last frame drawn
	done         bool

	viewStart int       // the conversation row at the top of the view
	conv      [][]span  // the conversation's rows at the last frame
	header    int       // the row of the prompt pinned at the top of the view; -1 for none
	sel       selection // text selected with the mouse in the conversation
	inputDrag bool      // the button went down in the input box and is still down
	clicks    int       // presses in a row at one cell: 1, 2 or 3
	lastPress time.Time
	pressAt   [2]int
	suggCache suggestCache
	copier    func(text string) (via string, err error) // tests replace the clipboard
	paster    func() (string, error)                    // and what is read from it
	// rightPaste is whether a right click pastes: in a console window of
	// its own (consoleWindow). A terminal pastes by itself.
	rightPaste bool
}

// panel is a dialog drawn above the input box, which takes the keys while
// it is open: an approval, the effort slider, a choice, the btw bubble.
type panel interface {
	rows(f *fullscreen, width int) [][]span
	// key handles a key; it returns true when the panel is done.
	key(f *fullscreen, p keyPress) bool
	// click handles a click on one of its rows; true when it is done.
	click(f *fullscreen, row int) bool
}

// wheeler is a panel that scrolls with the mouse wheel.
type wheeler interface {
	wheel(f *fullscreen, up bool)
}

// openPanel opens a panel. An approval, or the model's questions, cover
// the panel open, which comes back when they close.
func (f *fullscreen) openPanel(p panel) {
	switch p.(type) {
	case *approvalPanel, *askPanel:
		if f.panel != nil {
			f.under = f.panel
		}
	}
	f.panel = p
}

// pasting reports whether pasted text has somewhere to go: the input box,
// or a panel that takes it.
func (f *fullscreen) pasting() bool {
	_, takes := f.panel.(pasteTaker)
	return f.panel == nil || f.noting() || takes
}

// closePanel closes the panel, showing again the one it covered.
func (f *fullscreen) closePanel() {
	f.panel, f.under = f.under, nil
}

// runFullscreen runs the chat in the fullscreen UI until the user leaves.
// It returns false, before drawing anything, when the terminal can't do
// it, and the caller falls back to line mode.
func runFullscreen(c *chat) (code int, ok bool) {
	if c.openKeys == nil || c.term.out == nil {
		return 0, false
	}
	kb, err := c.openKeys(inputOptions{mouse: true, ctrlC: true})
	if err != nil {
		return 0, false
	}
	f := newFullscreen(c, kb, func() (int, int) { return fitScreen(c.term.out) })
	if c.opts.MCP != nil {
		c.opts.MCP.Subscribe(func(e mcp.Event) {
			f.post(func() {
				k := inkGray
				if e.Failed {
					k = inkYellow
				}
				f.say(e.Text, k)
			})
		})
	}
	return f.run(), true
}

func newFullscreen(c *chat, kb keyboard, size func() (int, int)) *fullscreen {
	f := &fullscreen{
		chat:       c,
		kb:         kb,
		scr:        screen{out: c.out, color: c.term.Color, trueColor: c.term.TrueColor},
		posts:      make(chan func(), 256),
		convo:      convo{tr: transcript{t: c.term}, tools: map[*agent.ToolRun]*entry{}},
		views:      map[int]*subagentView{},
		stripFocus: -1,
		ed:         newEditor(c.opts.History),
		size:       size,
		completion: -1,
		rightPaste: consoleWindow(),
	}
	f.tr.add(&bannerBlock{detail: f.bannerDetail(), dir: c.opts.Workspace})
	for _, n := range c.opts.Notices {
		if rest, ok := strings.CutPrefix(n, "✓ "); ok {
			n = c.term.sym().check + " " + rest
		}
		f.tr.add(note(n, inkGray))
	}
	for _, w := range c.opts.Warnings {
		f.tr.add(note("warning: "+w, inkYellow))
	}
	if c.opts.Resume != "" {
		f.resume(c.opts.Resume)
	}
	return f
}

// bannerDetail is the banner's model line: the model, its effort and the
// provider.
func (f *fullscreen) bannerDetail() string {
	s := f.opts.Status
	detail := s.Model
	if e := f.effortShown(); e != "" {
		detail += " · effort " + e
	}
	return detail + " · " + s.Provider
}

// refreshBanner redraws the banner after the model or effort changed.
func (f *fullscreen) refreshBanner() {
	if len(f.tr.entries) > 0 {
		if b, ok := f.tr.entries[0].b.(*bannerBlock); ok {
			b.detail = f.bannerDetail()
			f.tr.entries[0].changed()
		}
	}
}

func (f *fullscreen) run() int {
	io.WriteString(f.out, enterFullscreen)
	f.out.Flush()
	defer func() {
		f.kb.close()
		io.WriteString(f.out, leaveFullscreen)
		f.out.Flush()
		if f.cancel != nil {
			f.cancel()
		}
		if f.btwCancel != nil {
			f.btwCancel()
		}
	}()
	if f.opts.ConfirmYolo {
		f.confirmYolo(true)
	}
	if f.opts.ConfirmWebYolo {
		f.confirmWebYolo(true)
	}
	tick := time.NewTicker(80 * time.Millisecond)
	defer tick.Stop()
	f.draw()
	// Frames are drawn at most every minFrame: a reply streaming at a
	// hundred tokens a second is drawn sixty times a second, not a hundred,
	// and the last change always gets its frame from the timer.
	// A resize doesn't wait: the terminal has already reshaped the last
	// frame, with the input box no longer on the bottom rows.
	var later <-chan time.Time
	lastDraw := time.Now()
	for !f.done {
		resized := false
		select {
		case ev, ok := <-f.kb.events():
			if !ok {
				return ExitOK
			}
			f.handle(ev)
			resized = ev.kind == evResize
		case fn := <-f.posts:
			fn()
		case <-tick.C:
			f.frame++
			f.watchChrome()
			// A console doesn't always say its window changed size:
			// conhost is silent when the window shrank but not its buffer.
			if w, h := f.size(); w != f.scr.width || h != f.scr.height {
				resized = true
			} else if !f.animating() {
				continue
			}
		case <-later:
			later = nil
		case <-f.interrupts: // Ctrl+Break
			f.ctrlC()
		case <-f.chat.quit:
			return ExitInterrupted
		}
		f.drain()
		if wait := minFrame - time.Since(lastDraw); wait > 0 && !resized {
			if later == nil {
				later = time.After(wait)
			}
			continue
		}
		f.draw()
		lastDraw = time.Now()
	}
	return ExitOK
}

// minFrame is the shortest time between two frames.
const minFrame = 16 * time.Millisecond

// drain runs what the agent posted meanwhile, so a burst of streamed text
// is drawn in one frame.
func (f *fullscreen) drain() {
	for {
		select {
		case fn := <-f.posts:
			fn()
		default:
			return
		}
	}
}

// animating reports whether something on screen moves: the activity line,
// a running tool's clock, the slider's sparkles, a passing hint.
func (f *fullscreen) animating() bool {
	if f.busy || f.panel != nil || f.hint != "" || f.activeSubagents() > 0 {
		return true
	}
	// The editor and yolo labels shimmer.
	switch f.opts.Session.Mode() {
	case config.ApprovalEditor, config.ApprovalYolo:
		return true
	}
	return f.opts.Web != nil && f.opts.Web.Mode() == config.WebYolo
}

// say adds a note to the conversation.
func (f *fullscreen) say(text string, k ink) {
	f.tr.add(note(text, k))
	f.scroll = 0
}

// flash shows a passing message in the status line.
func (f *fullscreen) flash(text string) {
	f.hint, f.hintUntil = text, time.Now().Add(2500*time.Millisecond)
}

// The drawing.

var spinnerFrames = []string{"·", "∙", "•", "∙"}

func (f *fullscreen) draw() {
	w, h := f.size()
	if w <= 0 || h <= 0 {
		w, h = f.term.columns(), 24
	}
	if w != f.width || h != f.height {
		if w != f.width {
			f.sel = selection{} // the rows wrap anew
		}
		f.width, f.height = w, h
		f.scr.resize(w, h)
	}
	if f.hint != "" && time.Now().After(f.hintUntil) {
		f.hint = ""
	}

	// From the bottom up: the status line, the subagents' strip, the input
	// box, queued messages, the panel, the activity line; the conversation
	// gets the rest. A short screen, as a terminal in an editor's panel can
	// be, keeps what is needed most (squeeze).
	status := f.statusRow(w)
	panelOpen := f.panel != nil && !f.noting()
	text, cy, cx := f.inputText(w, max(1, min(10, h/3)))
	var activity []span
	if f.busy && f.viewing == nil { // a subagent's view has its own header
		activity = f.activityRow()
	}
	strip := f.subagentRow(w)
	var queued [][]span
	for _, q := range f.queue {
		queued = append(queued, []span{grayed("  ↳ " + clip(strings.ReplaceAll(f.ed.display(q), "\n", " "), max(10, w-14)) + " (queued)")})
	}
	n := rowCounts{header: b2i(f.viewing != nil), activity: b2i(activity != nil), queued: len(queued),
		rules: 2, text: len(text), strip: b2i(strip != nil), status: 1}

	// The panel, or the command completions, above the input box: the rows
	// the rest leaves, but for a few of the conversation's.
	var panelRows [][]span
	f.panelCut = 0
	reserve := min(3, h/8)
	room := func() int {
		return max(0, h-n.header-n.activity-n.queued-n.rules-n.text-n.strip-n.status-reserve)
	}
	if f.panel != nil {
		// An open panel waits for an answer, and the input box for it, so
		// on a short screen the panel gets their rows: the conversation's,
		// the queue's and the activity line's, then the input box's.
		shrink := []func(){
			func() { reserve = 0 },
			func() { n.queued = 0 },
			func() { n.activity = 0 },
			func() { n.rules = 0 },
			func() { n.text = 0 },
			func() { n.strip = 0 },
		}
		for i := 0; ; i++ {
			f.panelLimit = room()
			panelRows = f.panel.rows(f, w)
			if len(panelRows) <= f.panelLimit || !panelOpen || i == len(shrink) {
				break
			}
			shrink[i]()
		}
		if len(panelRows) > f.panelLimit { // the options, at the bottom, stay
			f.panelCut = len(panelRows) - f.panelLimit
			panelRows = panelRows[f.panelCut:]
		}
	} else if list, _ := f.suggestions(); len(list) > 0 {
		f.panelLimit = room()
		panelRows = f.completionRows(list, w)
		if limit := f.panelLimit; len(panelRows) > limit { // the chosen one stays in sight
			f.panelCut = max(0, min(f.completion-limit+1, len(panelRows)-limit))
			panelRows = panelRows[f.panelCut : f.panelCut+limit]
		}
	}
	n.panel = len(panelRows)
	n.squeeze(h)
	if cut := len(panelRows) - n.panel; cut > 0 {
		f.panelCut += cut
		panelRows = panelRows[cut:]
	}
	f.panelRows = len(panelRows)
	queued = queued[len(queued)-n.queued:]
	if n.text > 0 && n.text < len(text) { // the cursor's rows stay in sight
		text, cy, cx = f.inputText(w, n.text)
	}

	viewH := n.conv
	cv := &f.convo
	if f.viewing != nil {
		cv = &f.viewing.convo
	}
	conv, owners := cv.tr.rows(w)
	if f.scroll > 0 && len(conv) > f.viewRows {
		f.scroll += len(conv) - f.viewRows // stay on the rows being read
	}
	f.viewRows = len(conv)
	f.scroll = max(0, min(f.scroll, len(conv)-viewH))
	end := len(conv) - f.scroll
	start := max(0, end-viewH)
	f.viewStart, f.conv = start, conv
	rows := make([]frameRow, 0, h)
	f.owners = f.owners[:0]
	for i := start; i < end; i++ {
		row := conv[i]
		if from, to, ok := f.sel.columns(i, w); ok {
			row = reverseCells(row, from, to)
		}
		rows = append(rows, frameRow{spans: row})
		f.owners = append(f.owners, owners[i])
	}
	// Scrolled back, the prompt the rows on screen answer stays in sight
	// at the top, dim, like Claude Code's; a click goes back to it.
	f.header = -1
	if f.scroll > 0 && len(rows) > 2 && f.viewing == nil {
		for i := start; i >= 0; i-- {
			if owners[i] == nil { // the blank row between two blocks
				continue
			}
			if u, ok := owners[i].b.(*userBlock); ok {
				if i < start && (i == 0 || owners[i-1] != owners[i]) {
					f.header = i
					asked := strings.ReplaceAll(u.text, "\n", " ")
					rows[0] = frameRow{spans: []span{grayed(" " + f.term.sym().pointer + " " + clip(asked, max(10, w-4)))}}
				}
				break
			}
		}
	}
	if f.scroll > 0 && len(rows) > 0 {
		label := fmt.Sprintf(" ↓ %d more below · ctrl+end ", f.scroll)
		pad := max(0, (w-textWidth(label))/2)
		rows[len(rows)-1] = frameRow{spans: []span{plain(strings.Repeat(" ", pad)), styled(label, look{fg: inkWhite, bg: rgbInk(rgb{0x3a, 0x3a, 0x3a})})}}
	}
	for len(rows) < viewH {
		rows = append(rows, frameRow{})
	}
	if n.header > 0 {
		rows = append([]frameRow{f.viewHeader(w)}, rows...)
		f.owners = append([]*entry{nil}, f.owners...)
		f.viewStart--
	}
	if n.activity > 0 {
		rows = append(rows, frameRow{spans: activity})
	}
	f.panelTop = len(rows)
	for _, r := range panelRows {
		rows = append(rows, frameRow{spans: r})
	}
	for _, r := range queued {
		rows = append(rows, frameRow{spans: r})
	}
	if n.rules > 0 {
		rows = append(rows, frameRow{spans: rule(w)})
	}
	f.inputTop, f.inputShown = len(rows), n.text
	for _, r := range text[:n.text] {
		rows = append(rows, frameRow{spans: r})
	}
	if n.rules > 0 {
		rows = append(rows, frameRow{spans: rule(w)})
	}
	f.stripTop = -1
	if n.strip > 0 {
		f.stripTop = len(rows)
		rows = append(rows, frameRow{spans: strip})
	}
	if n.status > 0 {
		rows = append(rows, frameRow{spans: status})
	}
	for i := range rows { // nothing may reach past the screen's edge
		rows[i].spans = cut(rows[i].spans, w)
	}

	fr := frame{rows: rows, cursorX: min(cx, w-1), cursorY: f.inputTop + cy, cursor: (f.panel == nil || f.noting()) && n.text > 0}
	f.shown = fr
	f.scr.draw(fr)
	f.out.Flush()
}

// noting reports whether the input box takes a note for an approval.
func (f *fullscreen) noting() bool {
	a, ok := f.panel.(*approvalPanel)
	return ok && a.noting
}

// rule is a horizontal line across the screen.
func rule(width int) []span { return []span{grayed(strings.Repeat("─", width))} }

// inputText draws the input box's text with its prompt, at most maxRows
// rows of it, and says where the cursor is in them; draw puts a rule above
// and below.
func (f *fullscreen) inputText(width, maxRows int) (rows [][]span, cy, cx int) {
	prompt := "> "
	if f.noting() {
		prompt = "note: "
	}
	pw := textWidth(prompt)
	lines, pos, chipped, owner := f.ed.layoutChips(width - pw - 1)
	from, to, selected := f.ed.selection()
	cur := pos[f.ed.cursor]
	if cur[0] < f.inputScroll {
		f.inputScroll = cur[0]
	}
	if cur[0] >= f.inputScroll+maxRows {
		f.inputScroll = cur[0] - maxRows + 1
	}
	f.inputScroll = max(0, min(f.inputScroll, len(lines)-maxRows))
	for i := f.inputScroll; i < len(lines) && i < f.inputScroll+maxRows; i++ {
		p := strings.Repeat(" ", pw)
		if i == 0 {
			p = prompt
		}
		var sel []bool
		if selected {
			sel = make([]bool, len(owner[i]))
			for j, at := range owner[i] {
				sel[j] = at >= from && at < to
			}
		}
		row := append([]span{bolded(p, inkCyan)}, inputSpans(lines[i], chipped[i], sel)...)
		if i == 0 && f.ed.empty() && !f.noting() {
			row = append(row, grayed(f.placeholder(width-pw-1)))
		}
		rows = append(rows, row)
	}
	return rows, cur[0] - f.inputScroll, pw + cur[1]
}

// chipLook draws a paste's chip in the input box.
var chipLook = look{fg: rgbInk(logoFrom), bold: true}

// inputSpans draws a row of the input box, chips' labels apart and the
// selected characters, if sel says which, in reverse video.
func inputSpans(row []rune, chipped, sel []bool) []span {
	var out []span
	same := func(i, j int) bool { return chipped[j] == chipped[i] && (sel == nil || sel[j] == sel[i]) }
	for i := 0; i < len(row); {
		j := i
		for j < len(row) && same(i, j) {
			j++
		}
		l := look{}
		if chipped[i] {
			l = chipLook
		}
		if sel != nil && sel[i] {
			l.reverse = true
		}
		out = append(out, styled(string(row[i:j]), l))
		i = j
	}
	return out
}

// rowCounts are the rows each part of the screen gets, from the top.
type rowCounts struct {
	header, conv, activity, panel, queued, rules, text, strip, status int
}

// squeeze fits the parts into h rows. The conversation gets what the rest
// leaves; when that is nothing, parts go, the least needed first: the
// queue, the subagent view's header, the activity line, the strip, the
// input box's rows but the cursor's, its rules, the panel's top rows, and
// last the status line or the input box, whichever is left alone on a
// screen of one row: the input box, to type in.
func (n *rowCounts) squeeze(h int) {
	over := func() int {
		return n.header + n.activity + n.panel + n.queued + n.rules + n.text + n.strip + n.status - h
	}
	for _, part := range []*int{&n.queued, &n.header, &n.activity, &n.strip} {
		*part = max(0, *part-max(0, over()))
	}
	if o := over(); o > 0 {
		n.text = max(min(n.text, 1), n.text-o)
	}
	if over() > 0 {
		n.rules = 0
	}
	n.panel = max(0, n.panel-max(0, over()))
	if over() > 0 && n.text > 0 {
		n.status = 0
	}
	n.text = max(0, n.text-max(0, over()))
	n.status = max(0, n.status-max(0, over()))
	n.conv = max(0, -over())
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (f *fullscreen) placeholder(room int) string {
	options := []string{"Ask anything · / for commands · ctrl+c twice to quit", "Ask anything · / for commands", "Ask anything"}
	switch {
	case f.viewing != nil:
		name := clip(f.viewing.state.Description, 30)
		options = []string{"Message " + name + " · ← goes back", "Message " + name, "← goes back"}
	case f.busy:
		options = []string{"Type to queue a message · esc to interrupt", "Type to queue a message", "Queue a message"}
	}
	for _, o := range options {
		if textWidth(o) <= room {
			return o
		}
	}
	return options[len(options)-1]
}

// activityRow shows what the turn is doing, with a spinner and a shimmer
// moving across the words, how long it has run and how much has streamed.
func (f *fullscreen) activityRow() []span {
	pulse := 0.5 + 0.5*math.Sin(float64(f.frame)/2)
	spin := spinnerFrames[f.frame%len(spinnerFrames)]
	row := []span{plain("  "), styled(spin+" ", look{fg: rgbInk(logoFrom.mix(tealBright, pulse)), bold: true})}
	label := f.activity + "…"
	shine := f.frame % (len([]rune(label)) + 8)
	for i, r := range []rune(label) {
		c := rgb{0x9a, 0x9a, 0x9a}
		if d := math.Abs(float64(i - shine)); d < 3 {
			c = c.mix(rgb{0xff, 0xff, 0xff}, 1-d/3)
		}
		row = append(row, styled(string(r), look{fg: rgbInk(c)}))
	}
	info := []string{seconds(time.Since(f.turnStart))}
	if f.deltas > 0 {
		info = append(info, "↓ "+kTokens(f.deltas)+" tokens")
	}
	info = append(info, "esc to interrupt")
	return append(row, grayed(" ("+strings.Join(info, " · ")+")"))
}

// statusRow is the bottom line: the session on the left, a hint on the
// right.
func (f *fullscreen) statusRow(width int) []span {
	s := f.opts.Status
	var more []string
	if e := f.effortShown(); e != "" {
		more = append(more, "effort "+e)
	}
	more = append(more, f.lastUsage...)
	tail := ""
	if len(more) > 0 {
		tail = " · " + strings.Join(more, " · ")
	}
	mode, web, goal := f.modeLabel(), f.webLabel(), f.goalIndicator()
	line := func(parts ...[]span) []span {
		out := []span{plain("  ")}
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	// The richest line that fits; the mode always stays.
	lefts := [][]span{
		line(mode, []span{grayed(" (shift+tab to cycle)")}, web, []span{grayed(" · " + s.Provider + " · " + s.Model + tail)}, goal),
		line(mode, web, []span{grayed(" · " + s.Provider + " · " + s.Model + tail)}, goal),
		line(mode, web, []span{grayed(" · " + s.Model + tail)}, goal),
		line(mode, web, []span{grayed(" · " + s.Model)}, goal),
		line(mode, web, goal),
	}
	hint := f.hint
	_, btwOpen := f.panel.(*btwPanel)
	switch {
	case hint != "":
	case f.stripFocus >= 0:
		hint = "← → choose · enter opens · esc back"
	case f.viewing != nil:
		hint = "← back to the conversation"
	case f.btwAsking() != nil && !btwOpen:
		hint = "answering the side question · /btw to watch"
	case f.scroll > 0:
		hint = "PgUp/PgDn scroll · ctrl+end back to the end"
	case f.busy:
		hint = "esc to interrupt"
	}
	if f.stripFocus < 0 && f.viewing == nil && f.panel == nil && f.hint == "" && f.activeSubagents() > 0 && f.ed.empty() {
		if hint == "" {
			hint = "↓ subagents"
		} else {
			hint = "↓ subagents · " + hint
		}
	}
	// A hint for the moment outlasts all but the mode; the idle one,
	// "/help for commands", is the first thing to go.
	type layout struct{ left, right []span }
	var tries []layout
	if hint != "" {
		for _, left := range lefts {
			tries = append(tries, layout{left, []span{grayed(hint + "  ")}})
		}
	} else {
		tries = append(tries, layout{lefts[0], []span{grayed("/help for commands  ")}})
	}
	for _, left := range lefts {
		tries = append(tries, layout{left: left})
	}
	for _, t := range tries {
		if lw, rw := spansWidth(t.left), spansWidth(t.right); lw+rw+2 <= width || t.right == nil && lw <= width {
			if t.right == nil {
				return t.left
			}
			return append(append(t.left, plain(strings.Repeat(" ", width-lw-rw))), t.right...)
		}
	}
	return cut(lefts[len(lefts)-1], width)
}

// modeLabel is the approval mode as the status line starts with it, like
// Claude Code's "⏵⏵ accept edits on": a symbol and words in the mode's
// color, so a glance tells what the model may do unasked.
func (f *fullscreen) modeLabel() []span {
	sym := f.term.sym()
	switch f.opts.Session.Mode() {
	case config.ApprovalEditor:
		return shimmer(sym.edit+" editor mode on", editorGold, editorGlow, time.Now())
	case config.ApprovalAutoEdit:
		return []span{inked(sym.forward+" auto mode on", rgbInk(rgb{0xb4, 0x9c, 0xf8}))}
	case config.ApprovalPlan:
		return []span{inked(sym.pause+" plan mode on", rgbInk(tealBright))}
	case config.ApprovalYolo:
		return shimmer(sym.forward+" yolo mode on", yoloRed, yoloGlow, time.Now())
	}
	return []span{grayed("default mode")}
}

// webLabel shows web access in the status line, when it is on, and that
// web content is in the conversation.
func (f *fullscreen) webLabel() []span {
	if f.opts.Web == nil {
		return nil
	}
	var out []span
	switch f.opts.Web.Mode() {
	case config.WebSafe:
		out = append(out, grayed(" · "), inked("web safe", rgbInk(tealBright)))
	case config.WebYolo:
		out = append(append(out, grayed(" · ")), shimmer("web yolo", yoloRed, yoloGlow, time.Now())...)
	}
	if f.opts.Session.Tainted() {
		out = append(out, grayed(" · "), inked("tainted", inkYellow))
	}
	return out
}

// cycleMode moves to the next approval mode, as Shift+Tab does in Claude
// Code, from the least the model may do to the most: default, plan,
// editor, auto, yolo and back to default. Yolo is in the cycle without a
// question, at the user's request (2026-10-30), so it is a press away; its
// label shimmers red while it is on.
func (f *fullscreen) cycleMode() {
	s := f.opts.Session
	switch s.Mode() {
	case config.ApprovalDefault:
		s.SetMode(config.ApprovalPlan)
	case config.ApprovalPlan:
		s.SetMode(config.ApprovalEditor)
	case config.ApprovalEditor:
		s.SetMode(config.ApprovalAutoEdit)
	case config.ApprovalAutoEdit:
		s.SetMode(config.ApprovalYolo)
	default:
		s.SetMode(config.ApprovalDefault)
	}
}

// The input.

func (f *fullscreen) handle(ev event) {
	switch ev.kind {
	case evResize:
		f.scr.redraw()
	case evPaste:
		if pt, ok := f.panel.(pasteTaker); ok {
			pt.paste(f, ev.text)
		} else if f.panel == nil || f.noting() {
			f.ed.paste(ev.text)
			f.completion = -1
		}
	case evMouse:
		f.mouse(ev.mouse)
	case evKey:
		f.key(ev.key)
	}
}

func (f *fullscreen) key(p keyPress) {
	// Ctrl+Shift+C (where the terminal tells it from Ctrl+C: the Windows
	// console) and Ctrl+Insert copy: the text selected, or all of the input
	// box.
	if p.ctrl('c') && p.mod&modShift != 0 || p.k == keyInsert && p.mod&modCtrl != 0 {
		f.copyKey()
		return
	}
	if f.sel.active {
		switch {
		case p.ctrl('c'):
			f.copySelection() // with a selection, ctrl+c copies
			return
		case p.k == keyEsc, p.k == keyPgUp, p.k == keyPgDn, p.mod&(modShift|modAlt) != 0 && p.k != keyRune,
			(p.k == keyHome || p.k == keyEnd) && p.mod&modCtrl != 0:
			// these keep the selection
		default:
			f.sel = selection{}
		}
	}
	if f.ed.hasSelection() && (f.panel == nil || f.noting()) {
		switch {
		case p.ctrl('c'):
			f.copyInputSelection() // keeps the text and the selection
			return
		case p.k == keyEsc:
			f.ed.deselect()
			return
		}
	}
	if p.ctrl('c') {
		f.ctrlC()
		return
	}
	if p.ctrl('l') {
		f.scr.redraw()
		return
	}
	if (p.ctrl('v') || p.k == keyInsert && p.mod&modShift != 0) && f.pasting() {
		f.pasteClipboard() // a terminal that pastes itself never sends Ctrl+V
		return
	}
	if f.panel != nil && !f.noting() {
		if f.panel.key(f, p) {
			f.closePanel()
		}
		return
	}
	if f.noting() && (p.k == keyEnter && p.mod == 0 || p.k == keyEsc) {
		f.panel.key(f, p)
		f.closePanel()
		return
	}
	if f.stripKey(p) {
		return
	}
	if f.viewing != nil && (p.k == keyEsc || p.k == keyLeft && p.mod == 0 && f.ed.empty()) {
		f.closeView()
		return
	}
	if p.k == keyBackTab {
		f.cycleMode()
		return
	}
	if f.completionKey(p) {
		return
	}
	switch {
	case p.k == keyDown && p.mod == 0 && f.ed.empty() && f.ed.browse >= len(f.ed.history) && len(f.views) > 0:
		f.stripFocus = 0 // into the strip of subagents
	case p.k == keyEsc:
		if f.busy {
			f.interrupt()
		}
	case p.k == keyEnter && p.mod == 0:
		f.submit()
	case p.k == keyEnter, p.ctrl('j'):
		f.ed.insert("\n")
	case p.k == keyPgUp:
		f.scrollBy(max(1, f.viewHeight()/2))
	case p.k == keyPgDn:
		f.scrollBy(-max(1, f.viewHeight()/2))
	case p.k == keyHome && p.mod&modCtrl != 0:
		f.scroll = f.viewRows
	case p.k == keyEnd && p.mod&modCtrl != 0:
		f.scroll = 0
	case p.ctrl('o'):
		f.showThinking = !f.showThinking
		for _, e := range f.tr.entries {
			switch e.b.(type) {
			case *replyBlock, *toolBlock:
				e.changed()
			}
		}
	case p.ctrl('d'):
		if f.ed.empty() {
			f.done = true
		} else {
			f.ed.del()
		}
	default:
		f.edit(p)
	}
}

// edit applies an editing key to the input box. A movement key with Shift
// held selects, as in any editor: the selection grows from where the
// cursor was to where it goes, and typing replaces it.
func (f *fullscreen) edit(p keyPress) {
	width := f.inputWidth()
	e := f.ed
	shift := p.k != keyRune && p.mod&modShift != 0
	move := func(fn func()) {
		if shift {
			e.extend()
			f.sel = selection{} // the input box's selection is the one now
		} else {
			e.deselect()
		}
		fn()
	}
	switch {
	case p.k == keyRune && p.mod&(modCtrl|modAlt) == 0:
		e.insert(string(p.r))
	case p.k == keyBackspace && p.mod&(modCtrl|modAlt) != 0, p.ctrl('w'):
		e.killWord()
	case p.k == keyBackspace, p.ctrl('h'):
		e.backspace()
	case p.k == keyDelete:
		e.del()
	case p.k == keyLeft && p.mod&(modCtrl|modAlt) != 0, p.k == keyRune && p.mod == modAlt && p.r == 'b':
		move(e.wordLeft)
	case p.k == keyRight && p.mod&(modCtrl|modAlt) != 0, p.k == keyRune && p.mod == modAlt && p.r == 'f':
		move(e.wordRight)
	case p.k == keyLeft, p.ctrl('b'):
		move(e.left)
	case p.k == keyRight, p.ctrl('f'):
		move(e.right)
	case p.k == keyHome, p.ctrl('a'):
		move(e.home)
	case p.k == keyEnd, p.ctrl('e'):
		move(e.end)
	case shift && p.k == keyUp:
		move(func() { e.vertical(-1, width) })
	case shift && p.k == keyDown:
		move(func() { e.vertical(1, width) })
	case p.k == keyUp, p.ctrl('p'):
		move(func() { e.up(width) })
	case p.k == keyDown, p.ctrl('n'):
		move(func() { e.down(width) })
	case p.ctrl('u'):
		e.killStart()
	case p.ctrl('k'):
		e.killEnd()
	default:
		return
	}
	f.completion = -1
}

// promptWidth is the width of the input box's prompt, "> " or "note: ".
func (f *fullscreen) promptWidth() int {
	if f.noting() {
		return textWidth("note: ")
	}
	return 2
}

// inputWidth is the width the input box's text wraps at.
func (f *fullscreen) inputWidth() int { return f.width - f.promptWidth() - 1 }

func (f *fullscreen) viewHeight() int { return max(1, f.height-6) }

func (f *fullscreen) scrollBy(n int) {
	f.scroll = max(0, f.scroll+n)
}

// ctrlC interrupts a turn, closes a dialog or clears the input box; on an
// empty box it asks to be pressed again, and pressed again within two
// seconds it quits.
func (f *fullscreen) ctrlC() {
	now := time.Now()
	switch {
	case f.busy:
		f.interrupt()
	case f.panel != nil:
		if _, ok := f.panel.(*btwPanel); !ok {
			f.panel.key(f, keyPress{k: keyEsc})
		}
		f.closePanel()
	case !f.ed.empty():
		f.ed.clear()
		f.flash("cleared · ↑ brings it back · select text to copy it")
	case now.Sub(f.lastCtrlC) < 2*time.Second:
		f.done = true
	default:
		f.flash("press ctrl+c again to quit")
		f.lastCtrlC = now
		return
	}
	f.lastCtrlC = time.Time{}
}

func (f *fullscreen) interrupt() {
	if f.cancel != nil {
		f.cancel()
	}
}

func (f *fullscreen) mouse(m mouseEvent) {
	switch m.action {
	case mouseWheelUp, mouseWheelDown:
		up := m.action == mouseWheelUp
		if w, ok := f.panel.(wheeler); ok && m.y >= f.panelTop && m.y < f.panelTop+f.panelRows {
			w.wheel(f, up)
		} else if up {
			f.scrollBy(3)
		} else {
			f.scrollBy(-3)
		}
		return
	case mouseDrag:
		if f.inputDrag {
			f.dragInput(m.x, m.y)
		}
		if f.sel.dragging {
			y := m.y
			switch last := len(f.owners) - 1; {
			case y <= 0 && f.viewStart > 0:
				f.scrollBy(1) // dragging at the top edge scrolls back
				y = 0
			case y >= last && f.scroll > 0:
				f.scrollBy(-1)
				y = last
			case y > last:
				y = last
			}
			f.sel.head = cellPos{f.viewStart + max(0, y), max(0, m.x)}
			f.sel.active = f.sel.head != f.sel.anchor
		}
		return
	case mouseRelease:
		if f.inputDrag {
			f.inputDrag = false
			if f.ed.hasSelection() {
				f.copyInputSelection()
			} else {
				f.ed.deselect()
			}
		}
		if f.sel.dragging {
			f.sel.dragging = false
			if f.sel.active {
				f.copySelection()
			} else {
				f.clickConversation(m.y)
			}
		}
		return
	case mousePress:
		// A right click pastes, as in a console window, which can't while
		// the UI runs: its Quick Edit, which pastes, is off.
		if m.button == 2 && f.rightPaste && f.pasting() {
			f.pasteClipboard()
			return
		}
		if m.button != 0 {
			return
		}
	default:
		return
	}
	clicks := f.multiClick(m.x, m.y)
	f.ed.deselect() // a press anywhere ends the input box's selection
	switch {
	case f.panelRows > 0 && m.y >= f.panelTop && m.y < f.panelTop+f.panelRows:
		f.sel = selection{}
		row := m.y - f.panelTop + f.panelCut
		if f.panel != nil {
			if f.panel.click(f, row) {
				f.closePanel()
			}
		} else if list, start := f.suggestions(); row < len(list) {
			f.completion = row
			f.acceptCompletion(list, start)
		}
	case f.viewing != nil && m.y == 0:
		f.closeView() // its header: "← back"
	case f.stripTop >= 0 && m.y == f.stripTop:
		f.clickStrip(m.x)
	case m.y < len(f.owners):
		if f.scroll > 0 && m.y == len(f.owners)-1 {
			f.scroll, f.sel = 0, selection{} // the "more below" bar
			return
		}
		pos := cellPos{f.viewStart + m.y, m.x}
		switch clicks {
		case 1:
			f.sel = selection{anchor: pos, head: pos, dragging: true}
		case 2:
			f.selectWord(pos)
		case 3:
			f.selectLine(pos.row)
		}
	case m.y >= f.inputTop && m.y < f.inputTop+f.inputShown && (f.panel == nil || f.noting()):
		f.sel = selection{}
		f.clickInput(m.x, m.y-f.inputTop+f.inputScroll)
		switch clicks {
		case 1:
			f.ed.anchor, f.inputDrag = f.ed.cursor, true // a drag selects from here
		case 2:
			if f.ed.selectWordAt(f.ed.cursor) {
				f.copyInputSelection()
			}
		case 3:
			if !f.ed.empty() {
				f.ed.selectAll()
				f.copyInputSelection()
			}
		}
	default:
		f.sel = selection{}
	}
}

// clickConversation acts on a click in the conversation that selected
// nothing: the pinned prompt goes back to it, a folded thought or tool
// output unfolds, or folds again.
func (f *fullscreen) clickConversation(y int) {
	if y < 0 || y >= len(f.owners) {
		return
	}
	if y == 0 && f.header >= 0 {
		f.scroll = max(0, f.viewRows-f.header-len(f.owners))
		return
	}
	e := f.owners[y]
	if e == nil {
		return
	}
	switch b := e.b.(type) {
	case *replyBlock:
		if strings.Contains(spansText(e.rows[0]), "Thought for") {
			f.key(keyPress{k: keyRune, r: 'o', mod: modCtrl})
		}
	case *toolBlock:
		if v := f.taskView(b); v != nil {
			f.openView(v) // a task call opens its subagent
			return
		}
		if b.foldable() {
			b.expanded = !b.expanded
			e.changed()
		}
	}
}

// taskView returns the view of a task call's subagent, if it is one.
func (f *fullscreen) taskView(b *toolBlock) *subagentView {
	if b.sub == nil {
		return nil
	}
	return f.views[b.sub.ID]
}

// clickInput puts the cursor where the input box was clicked.
func (f *fullscreen) clickInput(x, row int) {
	if at := f.ed.positionAt(f.inputWidth(), row, x-f.promptWidth()); at >= 0 {
		f.ed.cursor, f.ed.column = at, -1
	}
}

// dragInput extends the input box's selection to where the mouse is,
// dragged from a press in the box: above the box, to the start of the
// text; below it, to the end.
func (f *fullscreen) dragInput(x, y int) {
	switch {
	case y < f.inputTop:
		f.ed.cursor = 0
	case y >= f.inputTop+f.inputShown:
		f.ed.cursor = len(f.ed.text)
	default:
		f.clickInput(x, y-f.inputTop+f.inputScroll)
	}
	f.ed.column = -1
}

func spansText(spans []span) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.text)
	}
	return b.String()
}

// submit sends what was typed: a command, or a message for the model,
// queued while a turn runs. A line ending in a backslash continues on the
// next.
func (f *fullscreen) submit() {
	raw := f.ed.value() // with its chips, which the history keeps
	if strings.HasSuffix(raw, "\\") && f.ed.cursor == len(f.ed.text) {
		f.ed.backspace()
		f.ed.insert("\n")
		return
	}
	if strings.TrimSpace(raw) == "" {
		return
	}
	text := f.ed.expand(raw) // the pastes in full
	f.ed.remember(raw)
	f.ed.reset()
	f.completion = -1
	f.scroll = 0
	if f.opts.SaveHistory != nil {
		f.opts.SaveHistory(f.ed.savedHistory())
	}
	if strings.HasPrefix(strings.TrimSpace(raw), "/") { // typed: a pasted "/" starts no command
		if f.viewing != nil {
			f.closeView()
		}
		f.command(strings.TrimSpace(text))
		return
	}
	if v := f.viewing; v != nil {
		f.tellSubagent(v, text)
		return
	}
	if f.goal != nil {
		f.goal.paused, f.goal.quiet = false, 0 // the user's message resumes a paused goal
	}
	if f.busy {
		f.queue = append(f.queue, raw)
		return
	}
	f.sendTyped(raw)
}

// The turn.

func (f *fullscreen) send(text string) { f.sendAs(text, text) }

// sendTyped sends a message as the user typed it, with its chips: the
// conversation shows the chips' labels, the model gets the pastes in full,
// and files are attached for the mentions in what was typed, not in what
// was pasted.
func (f *fullscreen) sendTyped(raw string) {
	f.sendMessage(f.ed.display(raw), f.ed.expand(raw), f.ed.typed(raw))
}

func (f *fullscreen) sendAs(shown, text string) { f.sendMessage(shown, text, text) }

// sendMessage sends text to the model, showing shown as the user's
// message, or nothing when it is empty: the goal loop's messages aren't
// the user's. The user's own messages attach the files and resources that
// typed, what they typed, mentions.
func (f *fullscreen) sendMessage(shown, text, typed string) {
	var attach func(string) (tools.Attachment, error)
	if f.opts.Files != nil {
		attach = f.opts.Files.Attach
	}
	m := f.opts.MCP
	f.startTurn(shown, func(ctx context.Context) (string, []attached, error) {
		if shown == "" {
			return text, nil, nil
		}
		var resource func(string, string) (string, error)
		servers := map[string]bool{}
		if m != nil {
			servers = m.Servers()
			resource = func(server, uri string) (string, error) {
				rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				return m.ReadResource(rctx, server, uri)
			}
		}
		// expandMentions returns typed with the attachments after it.
		full, notes := expandMentions(typed, attach, resource, servers)
		return text + strings.TrimPrefix(full, typed), notes, nil
	})
}

// startTurn starts a turn. shown is the user's message as the conversation
// shows it ("" for none); prepare, on the turn's goroutine, makes the text
// the model gets, and notes what it attached, before the turn runs.
func (f *fullscreen) startTurn(shown string, prepare func(ctx context.Context) (string, []attached, error)) {
	var user *entry
	if shown != "" {
		user = f.tr.add(&userBlock{text: shown})
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.busy, f.cancel, f.turnStart, f.firstDelta, f.deltas, f.activity = true, cancel, time.Now(), time.Time{}, 0, "Thinking"
	f.turnTools = 0
	go func() {
		text, notes, err := prepare(ctx)
		if len(notes) > 0 && user != nil {
			f.post(func() {
				user.b.(*userBlock).attached = notes
				user.changed()
			})
		}
		if err != nil {
			f.post(func() { f.finish(agent.Result{}, err) })
			return
		}
		res, err := f.opts.Session.Send(ctx, text, f)
		f.post(func() { f.finish(res, err) })
	}()
}

// runPrompt runs an MCP prompt command: the server's prompt, filled in with
// the arguments typed, is sent as the user's message.
func (f *fullscreen) runPrompt(command, args string) {
	if f.busy {
		f.flash("wait for the turn to end, or press esc")
		return
	}
	m := f.opts.MCP
	f.startTurn(strings.TrimSpace(command+" "+args), func(ctx context.Context) (string, []attached, error) {
		pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		text, err := m.RunPrompt(pctx, command, args)
		if err != nil {
			return "", nil, err
		}
		n := strings.Count(text, "\n") + 1
		return text, []attached{{text: fmt.Sprintf("sent the MCP prompt's text · %d line%s", n, pluralS(n)), ok: true}}, nil
	})
}

func (f *fullscreen) post(fn func()) { f.posts <- fn }

// finish ends a turn: it reports how it ended, updates the status line and
// sends the next queued message.
func (f *fullscreen) finish(res agent.Result, err error) {
	f.busy = false
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	if a, ok := f.panel.(*approvalPanel); ok {
		a.answer(agent.Approval{}) // the turn is over; nobody waits for it
		f.closePanel()
	}
	switch {
	case errors.Is(err, context.Canceled):
		f.say("Interrupted.", inkYellow)
	case err != nil:
		f.say("error: "+errorText(err), inkRed)
	case res.Stopped != "":
		f.say("Stopped: "+res.Stopped+".", inkYellow)
	case res.StopReason == provider.StopMaxTokens:
		f.say("Stopped: the reply reached the output token limit.", inkYellow)
	case res.StopReason == provider.StopRefusal:
		f.say(refusalText(res.StopDetail), inkYellow)
	}
	if w := f.unsavedWarning(); w != "" {
		f.say("warning: "+w, inkYellow)
	}
	f.lastUsage = append(f.usage(res), f.costPart()...)
	next := func() { f.goalStep(err) }
	if u := res.Usage; u != nil && err == nil {
		if at := f.autoCompactAt(); at > 0 && u.Input+u.Output >= at {
			f.say(fmt.Sprintf("The conversation reached %s tokens of context; compacting it to make room (/autocompact).", kTokens(u.Input+u.Output)), inkGray)
			f.compact("", next)
			return
		}
	}
	next()
}

// sendQueued sends the next message typed while a turn ran.
func (f *fullscreen) sendQueued() {
	if len(f.queue) > 0 && !f.busy {
		next := f.queue[0]
		f.queue = f.queue[1:]
		f.sendTyped(next)
	}
}

// autoCompactAt is the context size at which the conversation is compacted.
func (f *fullscreen) autoCompactAt() int {
	return agent.AutoCompactAt(f.opts.AutoCompact, f.opts.Status.ContextWindow)
}

// compact summarizes the conversation on another goroutine, as a turn,
// which Esc interrupts; then runs next, if not nil.
func (f *fullscreen) compact(focus string, next func()) {
	ctx, cancel := context.WithCancel(context.Background())
	f.busy, f.cancel, f.turnStart, f.firstDelta, f.deltas, f.activity = true, cancel, time.Now(), time.Time{}, 0, "Compacting the conversation"
	window := f.opts.Status.ContextWindow
	go func() {
		res, err := f.opts.Session.Compact(ctx, focus, window, func(chars int) {
			f.post(func() { f.deltas = chars / 4 })
		})
		f.post(func() {
			f.busy = false
			if f.cancel != nil {
				f.cancel()
				f.cancel = nil
			}
			switch {
			case errors.Is(err, agent.ErrNothingToCompact):
				f.say("There is nothing to compact yet.", inkGray)
			case errors.Is(err, agent.ErrNotSmaller):
				f.say(fmt.Sprintf("The summary wasn't smaller than the conversation (about %s tokens), so the conversation stays as it is.", kTokens(res.Before)), inkGray)
			case errors.Is(err, context.Canceled):
				f.say("Compaction interrupted; the conversation is unchanged.", inkYellow)
			case err != nil:
				f.say("Compaction failed: "+err.Error(), inkYellow)
			default:
				f.tr.add(&dividerBlock{text: fmt.Sprintf("Conversation compacted · %d messages, about %s → %s tokens",
					res.Messages, kTokens(res.Before), kTokens(res.After))})
				f.lastUsage = nil
				f.scroll = 0
			}
			if next != nil {
				next()
			}
		})
	}()
}

// autoCompactCommand shows or sets when the conversation is compacted by
// itself.
func (f *fullscreen) autoCompactCommand(arg string) {
	if arg == "" {
		f.openPanel(&compactPanel{s: newAutoCompactSlider(f.opts.Status.ContextWindow, f.opts.AutoCompact)})
		return
	}
	if arg == "show" {
		arg = ""
	}
	msg, ok := autoCompact(&f.opts, arg)
	k := ink{}
	if !ok {
		k = inkYellow
	}
	f.say(msg, k)
}

// autoCompact runs /autocompact for either UI: with no argument it says
// when the conversation is compacted by itself; with a number of tokens
// (150000 or 150k), on for the default, or off, it changes that and saves
// it. It returns what to tell the user, and false for a bad argument.
func autoCompact(opts *ChatOptions, arg string) (string, bool) {
	at := func() int { return agent.AutoCompactAt(opts.AutoCompact, opts.Status.ContextWindow) }
	if arg == "" {
		if at() > 0 {
			return fmt.Sprintf("The conversation is compacted when its context reaches %s tokens.", kTokens(at())), true
		}
		return "Auto-compaction is off; /compact compacts the conversation now.", true
	}
	n, ok := setup.ParseTokens(arg)
	if !ok {
		return "Give a number of tokens such as 150k, or on or off.", false
	}
	opts.AutoCompact = n
	saved := ""
	if opts.Providers != nil {
		if err := opts.Providers.SetAutoCompact(opts.Status.Provider, n); err != nil {
			saved = " (for this session; not saved: " + err.Error() + ")"
		} else {
			saved = ", saved for " + opts.Status.Provider
		}
	}
	if n == 0 {
		return "Auto-compaction is off" + saved + ".", true
	}
	return fmt.Sprintf("The conversation is compacted when its context reaches %s tokens%s.", kTokens(at()), saved), true
}

// usage describes the turn that just ended for the status line: context
// use and speed.
func (f *fullscreen) usage(res agent.Result) []string {
	var parts []string
	output := f.deltas
	if u := res.Usage; u != nil {
		used := u.Input + u.Output
		if w := f.opts.Status.ContextWindow; w > 0 {
			parts = append(parts, fmt.Sprintf("ctx %d%% (%s/%s)", used*100/w, kTokens(used), kTokens(w)))
		} else {
			parts = append(parts, "ctx "+kTokens(used)+" tokens")
		}
		if u.Output > 0 {
			output = u.Output
		}
	}
	if !f.firstDelta.IsZero() && output > 1 {
		if gen := time.Since(f.firstDelta).Seconds(); gen >= minSpeedSeconds {
			parts = append(parts, fmt.Sprintf("%.1f tok/s", float64(output)/gen))
		}
	}
	if u := res.Usage; u != nil {
		if at := f.autoCompactAt(); at > 0 && u.Input+u.Output >= at*3/4 && u.Input+u.Output < at {
			parts = append(parts, fmt.Sprintf("compacts at %s", kTokens(at)))
		}
	}
	return parts
}

// The agent.Host methods, called on the turn's goroutine.

func (f *fullscreen) ReplyStart() {
	f.post(func() {
		f.replyStart(f)
		f.activity = "Thinking"
	})
}

func (f *fullscreen) Event(ev provider.Event) {
	if r, ok := ev.(provider.Retrying); ok {
		f.post(func() { f.activity = retryText(r) })
		return
	}
	f.post(func() {
		text, ok := f.event(ev)
		if !ok {
			return
		}
		f.activity = "Thinking"
		if text {
			f.activity = "Writing"
		}
		if f.firstDelta.IsZero() {
			f.firstDelta = time.Now()
		}
		f.deltas++
	})
}

func (f *fullscreen) ReplyEnd() { f.post(f.replyEnd) }

func (f *fullscreen) redact(s string) string {
	if f.opts.Redactor == nil {
		return s
	}
	return f.opts.Redactor.String(s)
}

// toolEntry returns the entry of a tool call, adding it on first sight.
func (f *fullscreen) toolEntry(run *agent.ToolRun, snap agent.ToolRun) (*entry, *toolBlock) {
	return f.entryFor(run, snap, &f.showThinking)
}

func (f *fullscreen) ToolStart(run *agent.ToolRun) {
	snap := *run
	f.post(func() {
		_, b := f.toolEntry(run, snap)
		b.state, b.started = toolRunning, time.Now()
		f.activity = "Running " + snap.Name
		f.turnTools++
	})
}

func (f *fullscreen) ToolEnd(run *agent.ToolRun) {
	snap := *run
	f.post(func() {
		_, b := f.toolEntry(run, snap)
		b.state = toolDone
		delete(f.tools, run)
		f.activity = "Thinking"
	})
}

// Approve shows the call and asks in a panel whether it may run.
func (f *fullscreen) Approve(ctx context.Context, run *agent.ToolRun) (agent.Approval, error) {
	snap := *run
	answer := make(chan agent.Approval, 1)
	var inView bool // the call asks in its subagent's view, where it stays
	f.post(func() {
		cv := &f.convo
		if v := f.views[snap.AgentID]; snap.AgentID != 0 && v != nil && f.viewing == v {
			cv, inView = &v.convo, true
		}
		e, b := cv.entryFor(run, snap, &f.showThinking)
		b.state = toolAsking
		f.openPanel(newApprovalPanel(snap, e, answer))
		f.activity = "Waiting for you"
		f.scroll = 0
	})
	// A subagent's call shows under its task call once answered.
	forget := func() {
		if snap.Agent == "" {
			return
		}
		f.post(func() {
			if e, ok := f.tools[run]; ok && !inView {
				f.tr.remove(e)
				delete(f.tools, run)
			}
		})
	}
	select {
	case a := <-answer:
		forget()
		return a, nil
	case <-ctx.Done():
		f.post(func() {
			if a, ok := f.panel.(*approvalPanel); ok && a.reply == answer {
				f.closePanel()
			}
		})
		forget()
		return agent.Approval{}, ctx.Err()
	}
}

// The commands.

// commandList is what /help lists and the completion offers. subs are a
// command's subcommands, which the completion offers after its name;
// words are other words that find it, since the completion also searches
// what the commands do.
var commandList = []command{
	{name: "/autocompact", args: "[tokens|on|off]", about: "when the conversation is compacted by itself",
		subs:  []subcommand{{"on", "compact by itself, at the default size", false}, {"off", "never compact by itself", false}},
		words: "context tokens summary size"},
	{name: "/btw", args: "[question]", about: "ask a side question; /btw alone lists them", words: "aside question"},
	{name: "/clear", about: "start a new conversation", words: "reset forget new"},
	{name: "/compact", args: "[focus]", about: "summarize the conversation to free context", words: "shrink tokens summary"},
	{name: "/effort", args: "[level]", about: "how hard the model reasons: a slider, or a level", words: "thinking reasoning"},
	{name: "/goal", args: "[condition|clear]", about: "keep working until a condition is met",
		subs: []subcommand{{"clear", "end the goal", false}}, words: "loop until"},
	{name: "/help", about: "the commands and keys", words: "keys shortcuts"},
	{name: "/mcp", args: "[add|remove|enable|disable|reconnect] [name]", about: "the MCP servers: list, add, remove, switch on or off",
		subs: []subcommand{{"add", "add a server: a command, or a URL", true}, {"add-json", "add a server from its JSON", true},
			{"remove", "remove a server", true}, {"enable", "switch a server on", true}, {"disable", "switch a server off", true},
			{"reconnect", "connect to a server again", true}, {"tools", "a server's tools", true}},
		words: "servers protocol"},
	{name: "/mode", args: "[mode]", about: "the approval mode: plan, default, editor, auto or yolo; alone, pick one",
		subs: []subcommand{{"plan", "reads and searches only; the model presents a plan", false},
			{"default", "asks before edits and commands", false}, {"editor", "edits without asking, runs no commands", false},
			{"auto", "edits without asking, asks before commands", false}, {"yolo", "runs edits and commands without asking", false}},
		words: "approval permissions ask"},
	{name: "/model", args: "[id]", about: "switch the model of the provider in use: pick from the server's list, or name one",
		words: "switch change pick list"},
	{name: "/resume", args: "[text]", about: "go back to an earlier conversation in this folder", words: "continue session history load"},
	{name: "/rename", args: "<name>", about: "name this conversation, for /resume", words: "title session"},
	{name: "/cost", args: "[model [in out]]", about: "what this session cost, by model; set a model's prices in USD per million tokens",
		words: "price money spend usage tokens dollars"},
	{name: "/provider", args: "[subcommand]", about: "list, add, edit, test, trust, use, default or remove providers",
		subs: []subcommand{{"list", "the providers", false}, {"add", "add a provider, step by step", false},
			{"edit", "change a provider, vision among it", true}, {"test", "check a provider", true},
			{"trust", "trust a server's certificate", true}, {"use", "switch to a provider", true},
			{"default", "the provider sessions start with", true}, {"remove", "remove a provider", true}},
		words: "model server llama vision"},
	{name: "/quit", about: "leave (or ctrl+c twice, or ctrl+d)", words: "exit"},
	{name: "/tools", about: "the tools the model can use"},
	{name: "/web", args: "[off|safe|yolo]", about: "web access: the browser tools, and whether they ask",
		subs: []subcommand{{"safe", "the browser tools ask before opening a site", false}, {"off", "no web tools", false},
			{"yolo", "the browser tools run without asking", false}},
		words: "internet browser access"},
	{name: "/browser", args: "[install]", about: "Chrome for the browser tools: is it connected; install sets it up step by step",
		subs:  []subcommand{{"install", "set up the Chrome extension, step by step", false}},
		words: "chrome extension setup youtube connect tab"},
}

// command is a slash command, for /help and the completion.
type command struct {
	name, args, about string
	subs              []subcommand
	words             string
}

// subcommand is the first argument a command may take.
type subcommand struct {
	name, about string
	args        bool // more arguments follow
}

func (f *fullscreen) command(cmd string) {
	name, arg, _ := strings.Cut(cmd, " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "/quit", "/exit":
		f.done = true
	case "/clear":
		if f.busy {
			f.flash("wait for the turn to end, or press esc")
			return
		}
		f.opts.Session.Reset()
		f.tr.entries = f.tr.entries[:1]
		f.forgetSubagents()
		f.lastUsage, f.scroll, f.goal = nil, 0, nil
		f.scr.redraw()
	case "/help":
		f.help()
	case "/effort":
		f.effortCommand(arg)
	case "/mode":
		f.modeCommand(arg)
	case "/model":
		f.modelCommand(arg)
	case "/resume":
		f.resumeCommand(arg)
	case "/cost":
		f.costCommand(arg)
	case "/rename":
		f.renameCommand(arg)
	case "/web":
		msg, ok, yolo := webCommand(&f.opts, arg)
		switch {
		case yolo:
			f.confirmWebYolo(false)
		case ok:
			f.say(msg, ink{})
		default:
			f.say(msg, inkYellow)
		}
	case "/browser":
		if arg == "install" || arg == "setup" {
			f.browserInstall()
			return
		}
		f.say(strings.Join(browserCommand(&f.opts), "\n"), ink{})
	case "/browser-install":
		f.browserInstall()
	case "/tools":
		f.toolsList()
	case "/provider":
		f.providerCommand(arg)
	case "/compact":
		if f.busy {
			f.flash("wait for the turn to end, or press esc")
			return
		}
		f.compact(arg, nil)
	case "/autocompact":
		f.autoCompactCommand(arg)
	case "/goal":
		f.goalCommand(arg)
	case "/btw":
		f.btwCommand(arg)
	case "/mcp":
		f.tr.add(&noteBlock{spans: f.mcpCommand(arg)})
		f.scroll = 0
	default:
		if strings.HasPrefix(name, "/mcp__") && f.opts.MCP != nil {
			f.runPrompt(name, arg)
			return
		}
		f.say("Unknown command "+name+"; /help lists the commands.", inkYellow)
	}
}

func (f *fullscreen) help() {
	f.tr.add(helpBlock{})
	f.scroll = 0
}

// helpKeys are the keys /help lists after the commands.
var helpKeys = []string{
	"Enter sends · alt+enter, ctrl+j or \\ at the end adds a line · ↑↓ history · esc interrupts",
	"PgUp/PgDn and the wheel scroll · ctrl+o shows thinking and output · ctrl+l redraws · ctrl+c clears, twice quits",
	"shift+tab cycles the approval mode: default, auto-edit, plan (yolo only through /mode yolo)",
	"@ attaches a file (Tab completes it) · drag to select and copy, double-click a word, triple-click a line",
}

// helpBlock lists the commands in columns that fit the longest name and
// arguments, each description wrapping under its own column; a narrow
// screen puts the description under the command instead.
type helpBlock struct{}

func (helpBlock) draw(width int, t Terminal) [][]span {
	nameW, argsW := 0, 0
	for _, c := range commandList {
		nameW, argsW = max(nameW, textWidth(c.name)), max(argsW, textWidth(c.args))
	}
	nameW, argsW = nameW+2, argsW+2
	var rows [][]span
	for _, c := range commandList {
		name := styled(c.name, look{bold: true})
		if 2+nameW+argsW+30 <= width {
			first := []span{plain("  "), name, plain(strings.Repeat(" ", nameW-textWidth(c.name))),
				grayed(c.args), plain(strings.Repeat(" ", argsW-textWidth(c.args)))}
			rows = append(rows, wrap([]span{plain(c.about)}, width, first, []span{plain(strings.Repeat(" ", 2+nameW+argsW))})...)
			continue
		}
		rows = append(rows, wrap([]span{name, grayed(" " + c.args)}, width, []span{plain("  ")}, []span{plain("    ")})...)
		rows = append(rows, wrap([]span{plain(c.about)}, width, []span{plain("    ")}, []span{plain("    ")})...)
	}
	rows = append(rows, nil)
	for _, k := range helpKeys {
		rows = append(rows, wrap([]span{grayed(k)}, width, []span{plain("  ")}, []span{plain("  ")})...)
	}
	return rows
}

func (f *fullscreen) toolsList() {
	s := f.opts.Session
	reg := s.Tools()
	if reg == nil {
		f.say("No tools in this session.", inkGray)
		return
	}
	var lines []string
	for _, name := range reg.Names() {
		tool, _ := reg.Lookup(name)
		line := fmt.Sprintf("%-16s %s", name, map[tools.Kind]string{tools.Read: "reads", tools.Edit: "changes files", tools.Execute: "runs commands"}[tool.Kind()])
		if !s.Available(tool.Kind()) {
			line += " (not in " + s.Mode() + " mode)"
		}
		lines = append(lines, line)
	}
	f.say(strings.Join(lines, "\n"), ink{})
}

func (f *fullscreen) effortCommand(arg string) {
	if arg == "" {
		f.openPanel(&effortPanel{s: newEffortSlider(f.opts.EffortLevels, f.opts.Session.Effort())})
		return
	}
	l, ok := effort.Parse(arg)
	if !ok {
		f.say(fmt.Sprintf("Unknown effort %q; choose %s.", arg, effortChoices(f.opts.EffortLevels)), inkYellow)
		return
	}
	f.opts.Session.SetEffort(l)
	f.refreshBanner()
	f.say("Reasoning effort: "+effortText(l, f.opts.EffortLevels), ink{})
}

// effortText describes a level as sent to the model.
func effortText(l effort.Level, levels []effort.Level) string {
	switch sent := effort.Fit(l, levels); {
	case l == effort.Default:
		return "the server's default"
	case l == effort.None:
		return "none (no reasoning)"
	case sent != l:
		return fmt.Sprintf("%s, sent as %s, the nearest level the model supports", l, sent)
	}
	return string(l)
}

// modeOrder lists the approval modes as /mode offers them, from the least
// the model may do unasked to the most.
var modeOrder = []string{config.ApprovalPlan, config.ApprovalDefault, config.ApprovalEditor, config.ApprovalAutoEdit, config.ApprovalYolo}

func (f *fullscreen) modeCommand(arg string) {
	s := f.opts.Session
	mode, ok := config.ParseApprovalMode(arg)
	switch {
	case arg == "":
		f.pickMode()
		return
	case !ok:
		f.say(fmt.Sprintf("Unknown mode %q; choose %s.", arg, config.ApprovalModeNames), inkYellow)
		return
	case mode == config.ApprovalYolo:
		f.confirmYolo(false)
		return
	default:
		s.SetMode(mode)
	}
	f.say("Approval mode: "+s.Mode()+" ("+modeDescriptions[s.Mode()]+")", ink{})
}

// pickMode offers the approval modes to choose from, the one in effect
// marked; yolo still asks for its confirmation.
func (f *fullscreen) pickMode() {
	s := f.opts.Session
	p := &choicePanel{title: []span{styled("Approval mode", look{bold: true}), grayed(" · now " + s.Mode() + " · shift+tab cycles")}}
	for i, m := range modeOrder {
		label := m + ": " + modeDescriptions[m]
		if m == s.Mode() {
			label += " (now)"
			p.sel = i
		}
		p.options = append(p.options, label)
	}
	p.options = append(p.options, "Keep "+s.Mode())
	p.chosen = func(f *fullscreen, i int) {
		switch {
		case i < 0 || i >= len(modeOrder) || modeOrder[i] == s.Mode():
			return
		case modeOrder[i] == config.ApprovalYolo:
			// Once this panel has closed: the confirmation is a panel too.
			f.post(func() { f.confirmYolo(false) })
			return
		}
		s.SetMode(modeOrder[i])
		f.say("Approval mode: "+s.Mode()+" ("+modeDescriptions[s.Mode()]+")", ink{})
	}
	f.openPanel(p)
}

// confirmYolo asks before yolo mode; at the start, a no falls back to the
// configured mode.
func (f *fullscreen) confirmYolo(atStart bool) {
	f.openPanel(&choicePanel{
		title: []span{inked("yolo mode runs every edit and command the model asks for without asking you,", inkRed),
			plain(" in the workspace and outside it.")},
		options: []string{"Turn on yolo mode", "Keep asking"},
		chosen: func(f *fullscreen, i int) {
			s := f.opts.Session
			if i == 0 {
				s.SetMode(config.ApprovalYolo)
			} else if atStart {
				s.SetMode(f.opts.FallbackMode)
			}
			f.say("Approval mode: "+s.Mode()+" ("+modeDescriptions[s.Mode()]+")", ink{})
		},
	})
}

// confirmWebYolo asks before web yolo; at the start, a no falls back to
// safe. Keeping safe comes first, so a hasty Enter keeps it.
func (f *fullscreen) confirmWebYolo(atStart bool) {
	keep := f.opts.Web.Mode()
	if atStart {
		keep = config.WebSafe
	}
	f.openPanel(&choicePanel{
		title:   []span{inked(webYoloWarning, inkRed)},
		options: []string{"Keep web access " + keep, "Turn on web yolo"},
		chosen: func(f *fullscreen, i int) {
			mode := f.opts.Web.Mode()
			switch {
			case i == 1:
				mode = config.WebYolo
			case atStart:
				mode = config.WebSafe
			}
			msg, ok, _ := setWeb(&f.opts, mode)
			if ok {
				f.say(msg, ink{})
			} else {
				f.say(msg, inkYellow)
			}
		},
	})
}

// providerCommand runs /provider. Listing and switching happen here; the
// subcommands that ask questions run in line mode, on the normal screen.
func (f *fullscreen) providerCommand(arg string) {
	m := f.opts.Providers
	if m == nil {
		f.say("There is no config file to keep providers in.", inkYellow)
		return
	}
	sub, name, _ := strings.Cut(arg, " ")
	name = strings.TrimSpace(name)
	switch sub {
	case "":
		f.providerPicker()
	case "list":
		f.say(strings.Join(append(m.List(f.opts.Status.Provider), providerUsage), "\n"), ink{})
	case "use":
		if name == "" {
			f.say("Name the provider: /provider use <name>", inkYellow)
			return
		}
		if f.busy {
			f.flash("wait for the turn to end, or press esc")
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		f.busy, f.cancel, f.turnStart, f.firstDelta, f.deltas, f.activity = true, cancel, time.Now(), time.Time{}, 0, "Connecting to "+name
		go func() {
			conn, err := f.opts.Use(ctx, name)
			f.post(func() {
				f.busy = false
				if f.cancel != nil {
					f.cancel()
					f.cancel = nil
				}
				switch {
				case errors.Is(err, context.Canceled):
					f.say("Interrupted; still using "+f.opts.Status.Provider+".", inkYellow)
				case err != nil:
					f.say("error: "+errorText(err), inkRed)
				default:
					for _, w := range conn.Warnings {
						f.say("warning: "+w, inkYellow)
					}
					f.opts.Status.Provider, f.opts.Status.Model = conn.Name, conn.Provider.Model
					f.opts.Status.ContextWindow, f.opts.EffortLevels = conn.ContextWindow, conn.EffortLevels
					f.opts.AutoCompact = conn.AutoCompact
					f.lastUsage = nil
					f.refreshBanner()
					f.say("Now using "+conn.Name+" · "+conn.Provider.Model+".", ink{})
				}
				f.sendQueued()
			})
		}()
	case "default":
		if err := m.SetDefault(name); err != nil {
			f.say("error: "+err.Error(), inkRed)
			return
		}
		f.say(name+" is now the default provider.", ink{})
	default:
		if f.busy {
			f.flash("wait for the turn to end, or press esc")
			return
		}
		var lines []said
		f.suspend(func() { lines = f.chat.providerCommand(arg) })
		f.refreshBanner()
		f.showSaid(lines)
	}
}

// providerPicker asks which provider to switch to, or to add one.
func (f *fullscreen) providerPicker() {
	if f.busy {
		f.flash("wait for the turn to end, or press esc")
		return
	}
	cfg := f.opts.Providers.Config
	names := providerNames(cfg)
	var options []string
	sel := 0
	for i, name := range names {
		options = append(options, name+"  "+providerDetail(cfg, name, f.opts.Status.Provider))
		if name == f.opts.Status.Provider {
			sel = i
		}
	}
	options = append(options, "Add a provider: set one up, step by step", "Close")
	f.openPanel(&choicePanel{title: []span{styled("Which provider?", look{bold: true})}, options: options, sel: sel,
		chosen: func(f *fullscreen, i int) {
			switch {
			case i == len(names):
				f.post(func() { f.providerCommand("add") }) // once the panel is closed
			case i > len(names): // closed
			case names[i] == f.opts.Status.Provider:
				f.flash("already using " + names[i])
			default:
				f.post(func() { f.providerCommand("use " + names[i]) })
			}
		}})
}

// suspend leaves the fullscreen UI for a command that asks questions in
// line mode, and comes back when it is done.
func (f *fullscreen) suspend(run func()) {
	f.kb.close()
	io.WriteString(f.out, leaveFullscreen)
	f.out.Flush()
	run()
	f.out.Flush()
	if kb, err := f.openKeys(inputOptions{mouse: true, ctrlC: true}); err == nil {
		f.kb = kb
	}
	io.WriteString(f.out, enterFullscreen)
	f.scr.redraw()
}

// The panels.

// optionRows draws a list of options, the chosen one marked.
func optionRows(f *fullscreen, options []string, sel, width int) [][]span {
	var rows [][]span
	for i, o := range options {
		l := look{}
		mark := "   "
		if i == sel {
			l = look{fg: inkCyan, bold: true}
			mark = " " + f.term.sym().pointer + " "
		}
		rows = append(rows, wrap([]span{styled(fmt.Sprintf("%s%d. ", mark, i+1), l), styled(o, l)}, width, nil, []span{plain("      ")})...)
	}
	return rows
}

// choiceKey moves through options; it returns the option chosen with Enter
// or its number, and -2 for Esc.
func choiceKey(p keyPress, sel *int, n int) int {
	switch {
	case p.k == keyUp:
		*sel = (*sel - 1 + n) % n
	case p.k == keyDown, p.k == keyTab:
		*sel = (*sel + 1) % n
	case p.k == keyEnter:
		return *sel
	case p.k == keyEsc:
		return -2
	case p.k == keyRune && p.r >= '1' && int(p.r-'0') <= n:
		return int(p.r - '1')
	}
	return -1
}

// choicePanel asks the user to choose one of a few options.
type choicePanel struct {
	title   []span
	options []string
	sel     int
	chosen  func(f *fullscreen, i int) // with -2 for Esc, which counts as the last option
}

func (c *choicePanel) rows(f *fullscreen, width int) [][]span {
	rows := [][]span{rule(width)}
	rows = append(rows, wrap(c.title, width, []span{plain(" ")}, []span{plain(" ")})...)
	return append(rows, optionRows(f, c.options, c.sel, width)...)
}

func (c *choicePanel) key(f *fullscreen, p keyPress) bool {
	i := choiceKey(p, &c.sel, len(c.options))
	if i == -1 {
		return false
	}
	if i == -2 {
		i = len(c.options) - 1
	}
	c.chosen(f, i)
	return true
}

func (c *choicePanel) click(f *fullscreen, row int) bool {
	i := row - (len(c.rows(f, f.width)) - len(c.options))
	if i < 0 || i >= len(c.options) {
		return false
	}
	c.chosen(f, i)
	return true
}

// approvalPanel asks whether a tool call may run, like Claude Code's
// permission prompt: yes, yes for the session, or no with an optional note
// typed in the input box.
type approvalPanel struct {
	run     agent.ToolRun
	entry   *entry
	reply   chan agent.Approval
	options []string
	sel     int
	noting  bool
	draft   string // what the input box held before the note
	// noSession leaves "yes for this session" out: no answer covers the
	// next call.
	noSession bool
}

func newApprovalPanel(run agent.ToolRun, e *entry, reply chan agent.Approval) *approvalPanel {
	session := "Yes, and don't ask again this session"
	switch {
	case run.Kind == tools.Read && len(run.Outside) > 0:
		session = "Yes, and don't ask again for reads outside the workspace this session"
	case run.Kind == tools.Edit && run.Question == "" && run.SessionFor != "":
		session = "Yes, and don't ask again for " + run.SessionFor + " this session"
	case run.Kind == tools.Edit:
		session = "Yes, and don't ask again for this file this session"
	case run.Kind == tools.Execute && strings.HasPrefix(run.Name, "mcp__"):
		session = "Yes, and don't ask again for this tool this session"
	case run.Kind == tools.Execute:
		session = "Yes, and don't ask again for this command this session"
	}
	if run.Question != "" {
		if run.SessionFor == "" {
			return &approvalPanel{run: run, entry: e, reply: reply, noSession: true,
				options: []string{"Yes", "No, and tell the model what to do instead"}}
		}
		session = "Yes, and don't ask again for " + run.SessionFor + " this session"
	}
	return &approvalPanel{run: run, entry: e, reply: reply,
		options: []string{"Yes", session, "No, and tell the model what to do instead"}}
}

func (a *approvalPanel) question() string {
	switch {
	case a.run.Question != "":
		return a.run.Question
	case a.run.Kind == tools.Edit:
		return "Apply this change?"
	case a.run.Kind == tools.Execute && strings.HasPrefix(a.run.Name, "mcp__"):
		return "Call this MCP tool?"
	case a.run.Kind == tools.Execute:
		return "Run this command?"
	case len(a.run.Outside) > 0:
		return "Read outside the workspace?"
	}
	return "Allow this?"
}

func (a *approvalPanel) rows(f *fullscreen, width int) [][]span {
	title := []span{plain(" "), bolded(a.question(), inkYellow)}
	if a.run.Tainted {
		title = append(title, grayed(" · web content is in the conversation"))
	}
	if a.run.Agent != "" {
		title = append(title, grayed(" · asked by the subagent "), plain(a.run.Agent))
	}
	rows := [][]span{rule(width), title}
	rows = append(rows, optionRows(f, a.options, a.sel, width)...)
	if a.noting {
		rows = append(rows, []span{grayed("   Type the note below and press Enter; Esc says no without one.")})
	}
	return rows
}

func (a *approvalPanel) key(f *fullscreen, p keyPress) bool {
	if a.noting {
		if p.k == keyEnter {
			a.answer(agent.Approval{Note: strings.TrimSpace(f.ed.expand(f.ed.value()))})
		} else {
			a.answer(agent.Approval{})
		}
		f.ed.set(a.draft)
		return true
	}
	return a.choose(f, choiceKey(p, &a.sel, len(a.options)))
}

func (a *approvalPanel) click(f *fullscreen, row int) bool {
	if i := row - 2; i >= 0 && i < len(a.options) {
		a.sel = i
		return a.choose(f, i)
	}
	return false
}

// choose acts on an option; Esc refuses without a note.
func (a *approvalPanel) choose(f *fullscreen, i int) bool {
	if a.noSession && i >= 1 {
		i++ // past the answer that isn't there
	}
	switch i {
	case -1:
		return false
	case 0:
		a.answer(agent.Approval{Allow: true})
	case 1:
		a.answer(agent.Approval{Allow: true, Session: true})
	case 2:
		a.noting, a.draft = true, f.ed.value()
		f.ed.reset()
		return false
	default:
		a.answer(agent.Approval{})
	}
	return true
}

// answer replies to the agent, once, and notes the answer on the call.
func (a *approvalPanel) answer(ap agent.Approval) {
	if a.reply == nil {
		return
	}
	a.reply <- ap
	a.reply = nil
	if b, ok := a.entry.b.(*toolBlock); ok {
		switch {
		case ap.Allow && ap.Session:
			b.answer = "allowed for the session"
		case ap.Allow:
			b.answer = "allowed"
		}
		b.run.Asked = true
		a.entry.changed()
	}
}

// effortPanel is the /effort slider.
type effortPanel struct {
	s *effortSlider
}

func (e *effortPanel) rows(f *fullscreen, width int) [][]span {
	return append([][]span{rule(width)}, e.s.rows(f.term, f.frame, width)...)
}

func (e *effortPanel) key(f *fullscreen, p keyPress) bool {
	done, ok, save := e.s.key(p)
	if !done {
		return false
	}
	if !ok {
		f.say("Reasoning effort stays "+e.s.nowName+".", inkGray)
		return true
	}
	choice := e.s.stops[e.s.pos]
	f.opts.Session.SetEffort(choice.level)
	f.refreshBanner()
	line := []span{plain("Reasoning effort: " + choice.name)}
	switch {
	case !save:
		line = append(line, grayed(", for this session"))
	case f.opts.Providers == nil:
		line = append(line, grayed(", for this session (there is no config file to save it in)"))
	default:
		if err := f.opts.Providers.SetEffort(f.opts.Status.Provider, choice.level); err != nil {
			line = append(line, inked(", for this session; not saved: "+err.Error(), inkYellow))
		} else {
			line = append(line, grayed(", saved for "+f.opts.Status.Provider))
		}
	}
	f.tr.add(&noteBlock{spans: line})
	return true
}

func (e *effortPanel) click(f *fullscreen, row int) bool { return false }
