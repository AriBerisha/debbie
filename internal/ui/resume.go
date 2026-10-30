package ui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"debbie/internal/agent"
	"debbie/internal/provider"
	"debbie/internal/sessions"
	"debbie/internal/setup"
)

// /resume (SPEC §12): every conversation is saved as it goes, in a folder
// for the workspace. /resume lists the others, the most recent first, and
// the one picked takes the place of the conversation, which stays saved;
// it goes on in its own file. --continue resumes the latest at the start.

// maxReplayed is how many steps of a resumed conversation the fullscreen
// UI shows again: the latest ones.
const maxReplayed = 300

// sessionAge says in words how long before now t was.
func sessionAge(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return count(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return count(int(d.Hours()), "hour") + " ago"
	case d < 7*24*time.Hour:
		return count(int(d.Hours()/24), "day") + " ago"
	}
	return t.Format("2 Jan 2006")
}

func count(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// sessionDetail describes a saved session beside its title.
func sessionDetail(s sessions.Info, now time.Time) string {
	parts := []string{sessionAge(s.Updated, now), count(s.Messages, "message")}
	if s.Model != "" {
		parts = append(parts, s.Model)
	}
	return strings.Join(parts, " · ")
}

// otherSessions lists the saved sessions of the workspace but the one in
// progress, the most recent first.
func (c *chat) otherSessions() ([]sessions.Info, error) {
	if c.opts.Sessions == nil {
		return nil, errors.New("conversations aren't saved in this session")
	}
	all, err := sessions.List(c.opts.Sessions.Dir())
	if err != nil {
		return nil, err
	}
	current := c.opts.Sessions.Path()
	var out []sessions.Info
	for _, s := range all {
		if s.Path != current {
			out = append(out, s)
		}
	}
	return out, nil
}

// loadSession makes the session in path the conversation.
func (c *chat) loadSession(path string) ([]provider.Message, error) {
	history, tainted, err := sessions.Load(path)
	if err != nil {
		return nil, err
	}
	c.opts.Session.Resume(history, tainted)
	c.opts.Sessions.Resume(path, history, tainted)
	c.lastUsage, c.goal = nil, nil
	return history, nil
}

// unsavedWarning says, once, that the conversation can't be saved.
func (c *chat) unsavedWarning() string {
	if c.unsavedWarned {
		return ""
	}
	if err := c.opts.Sessions.Err(); err != nil {
		c.unsavedWarned = true
		return "this conversation isn't being saved for /resume: " + err.Error()
	}
	return ""
}

// Line mode.

// resumeCommand lists the saved sessions to pick one; with text, it lists
// those whose title contains it.
func (c *chat) resumeCommand(arg string) {
	list, err := c.otherSessions()
	if err != nil {
		c.r.println(c.st.yellow(err.Error()))
		return
	}
	list = filterSessions(list, arg)
	if len(list) == 0 {
		c.r.println(c.st.gray("No other saved conversations for this folder yet."))
		return
	}
	list = list[:min(len(list), 30)]
	now := time.Now()
	ch := setup.Choice{Question: "Resume which conversation?"}
	for _, s := range list {
		ch.Options = append(ch.Options, setup.Option{Label: s.Title, Detail: sessionDetail(s, now)})
	}
	i, _, err := c.Choose(ch)
	if err != nil {
		c.r.println(c.st.yellow("Cancelled; the conversation stays as it is."))
		return
	}
	c.resume(list[i].Path)
}

// resume resumes the session in path and shows how it ended.
func (c *chat) resume(path string) {
	history, err := c.loadSession(path)
	if err != nil {
		c.r.println(c.st.red("error: " + err.Error()))
		return
	}
	steps := agent.Replay(history)
	c.r.println(c.st.gray(fmt.Sprintf("Resumed %q: %s. Where it left off:", sessions.Title(history), count(len(history), "message"))))
	var lastUser, lastText string
	for _, s := range steps {
		if s.User != "" {
			lastUser, lastText = s.User, ""
		}
		if s.Text != "" {
			lastText = s.Text
		}
	}
	if lastUser != "" {
		c.r.println(c.st.cyan("> ") + firstLine(lastUser, 200))
	}
	if lastText != "" {
		c.r.println(lastText)
	}
}

func filterSessions(list []sessions.Info, filter string) []sessions.Info {
	q := strings.ToLower(strings.TrimSpace(filter))
	if q == "" {
		return list
	}
	var out []sessions.Info
	for _, s := range list {
		if strings.Contains(strings.ToLower(s.Title), q) {
			out = append(out, s)
		}
	}
	return out
}

// The fullscreen UI.

func (f *fullscreen) resumeCommand(arg string) {
	if f.busy {
		f.flash("wait for the turn to end, or press esc")
		return
	}
	list, err := f.otherSessions()
	switch {
	case err != nil:
		f.say(err.Error(), inkYellow)
	case len(list) == 0:
		f.say("No other saved conversations for this folder yet. Each one is saved as it goes, for /resume and --continue.", inkGray)
	default:
		f.openPanel(&resumePanel{items: list, filter: []rune(arg), now: time.Now()})
	}
}

// resume resumes the session in path and shows it again.
func (f *fullscreen) resume(path string) {
	history, err := f.loadSession(path)
	if err != nil {
		f.say("error: "+err.Error(), inkRed)
		return
	}
	f.tr.entries = f.tr.entries[:1] // the banner stays
	f.forgetSubagents()
	f.scroll = 0
	steps := agent.Replay(history)
	if skip := len(steps) - maxReplayed; skip > 0 {
		f.say(fmt.Sprintf("(%d earlier steps aren't shown again.)", skip), inkGray)
		steps = steps[skip:]
	}
	for _, s := range steps {
		switch {
		case s.Summarized:
			f.say("(The conversation before this was summarized when it was compacted.)", inkGray)
		case s.User != "":
			text := s.User
			if s.Attached > 0 {
				text += fmt.Sprintf("\n(%s attached)", count(s.Attached, "file"))
			}
			f.tr.add(&userBlock{text: text})
		default:
			if s.Text != "" {
				b := &replyBlock{done: true, unfold: &f.showThinking}
				b.text.WriteString(s.Text)
				f.tr.add(b)
			}
			for _, run := range s.Calls {
				f.tr.add(&toolBlock{run: run, state: toolDone, details: &f.showThinking})
			}
		}
	}
	f.say(fmt.Sprintf("Resumed %q: %s. It goes on from here.", sessions.Title(history), count(len(history), "message")), inkGray)
	f.refreshBanner()
	f.scr.redraw()
}

// resumePanel lists the saved conversations: ↑ ↓ choose, Enter resumes,
// typing narrows the list by title, Esc closes it.
type resumePanel struct {
	items   []sessions.Info
	now     time.Time
	filter  []rune
	sel     int
	top     int
	visible int
	owners  []int // the item each row shows, or -1
}

func (p *resumePanel) matched() []int {
	q := strings.ToLower(strings.TrimSpace(string(p.filter)))
	var out []int
	for i, s := range p.items {
		if q == "" || strings.Contains(strings.ToLower(s.Title), q) {
			out = append(out, i)
		}
	}
	return out
}

func (p *resumePanel) choose(f *fullscreen) bool {
	matched := p.matched()
	if len(matched) == 0 {
		return false
	}
	p.sel = max(0, min(p.sel, len(matched)-1))
	path := p.items[matched[p.sel]].Path
	f.post(func() { f.resume(path) }) // once the panel is closed
	return true
}

func (p *resumePanel) key(f *fullscreen, k keyPress) bool {
	n := len(p.matched())
	switch {
	case k.k == keyEsc:
		return true
	case k.k == keyEnter:
		return p.choose(f)
	case k.k == keyUp, k.k == keyBackTab:
		p.sel--
	case k.k == keyDown, k.k == keyTab:
		p.sel++
	case k.k == keyPgUp:
		p.sel -= max(1, p.visible-1)
	case k.k == keyPgDn:
		p.sel += max(1, p.visible-1)
	case k.k == keyHome:
		p.sel = 0
	case k.k == keyEnd:
		p.sel = n - 1
	case k.k == keyBackspace:
		if len(p.filter) > 0 {
			p.filter, p.sel = p.filter[:len(p.filter)-1], 0
		}
	case k.ctrl('u'):
		p.filter, p.sel = nil, 0
	case k.k == keyRune && k.mod&(modCtrl|modAlt) == 0 && (k.r > ' ' || k.r == ' ' && len(p.filter) > 0):
		p.filter, p.sel = append(p.filter, k.r), 0
	}
	p.sel = max(0, min(p.sel, n-1))
	return false
}

func (p *resumePanel) paste(f *fullscreen, text string) {
	p.filter, p.sel = append(p.filter, []rune(strings.Join(strings.Fields(text), " "))...), 0
}

func (p *resumePanel) click(f *fullscreen, row int) bool {
	if row < 0 || row >= len(p.owners) || p.owners[row] < 0 {
		return false
	}
	for i, m := range p.matched() {
		if m == p.owners[row] {
			p.sel = i
			return p.choose(f)
		}
	}
	return false
}

func (p *resumePanel) wheel(f *fullscreen, up bool) {
	if up {
		p.sel--
	} else {
		p.sel++
	}
	p.sel = max(0, min(p.sel, len(p.matched())-1))
}

func (p *resumePanel) rows(f *fullscreen, width int) (rows [][]span) {
	defer func() {
		for i := range rows {
			rows[i] = cut(rows[i], width)
		}
	}()
	p.owners = p.owners[:0]
	add := func(owner int, r []span) {
		rows = append(rows, r)
		p.owners = append(p.owners, owner)
	}
	sym := f.term.sym()
	add(-1, rule(width))
	add(-1, []span{plain(" "), styled("Resume a conversation", look{bold: true}), grayed(" · this conversation stays saved")})
	matched := p.matched()
	if len(matched) == 0 {
		add(-1, []span{inked(fmt.Sprintf(" No saved conversation's title has %q.", strings.TrimSpace(string(p.filter))), inkYellow)})
	}
	fixed := 4 // the rule, the title, the filter line and the hints
	p.visible = max(1, min(f.panelLimit, max(8, f.height*2/3))-fixed)
	p.sel = max(0, min(p.sel, len(matched)-1))
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+p.visible {
		p.top = p.sel - p.visible + 1
	}
	p.top = max(0, min(p.top, len(matched)-p.visible))
	for j := p.top; j < len(matched) && j < p.top+p.visible; j++ {
		s := p.items[matched[j]]
		l, mark := look{}, "   "
		if j == p.sel {
			l, mark = look{fg: inkCyan, bold: true}, " "+sym.pointer+" "
		}
		detail := "  " + sessionDetail(s, p.now)
		title := clip(s.Title, max(10, width-textWidth(mark)-textWidth(detail)-1))
		add(matched[j], []span{styled(mark, l), styled(title, l), grayed(detail)})
	}
	cursor := styled(" ", look{reverse: true})
	if len(p.filter) == 0 {
		add(-1, []span{grayed("   type to search the titles "), cursor})
	} else {
		add(-1, []span{grayed("   search: "), plain(string(p.filter)), cursor})
	}
	hints := "↑↓ choose · enter resumes · esc cancel"
	if len(matched) > p.visible {
		hints += fmt.Sprintf(" · %d of %d", p.sel+1, len(matched))
	}
	add(-1, []span{grayed("   " + hints)})
	return rows
}
