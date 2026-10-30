package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"debbie/internal/provider"
	"debbie/internal/setup"
)

// /model switches the model of the provider in use, as Claude Code's does:
// alone it lists the models the server offers and the user picks one; with
// text it switches to that model when the server lists it, and otherwise
// lists the models whose ID or name contains the text. The choice is saved
// as the provider's model, so the next session starts with it; /provider
// use switches providers.

// modelLabel is how a model shows in a list: Claude's by its display name
// with its ID after it, a server's by its ID; the context size follows when
// it is known.
func modelLabel(m provider.ModelInfo) (label, detail string) {
	label = m.ID
	if m.Name != "" {
		label = m.Name
		detail = m.ID
	}
	if m.ContextWindow > 0 {
		if detail != "" {
			detail += " · "
		}
		detail += kTokens(m.ContextWindow) + " context"
	}
	return label, detail
}

// filterModels returns the indexes of the models whose ID or name contains
// filter, ignoring case; all of them for an empty filter.
func filterModels(models []provider.ModelInfo, filter string) []int {
	q := strings.ToLower(strings.TrimSpace(filter))
	var out []int
	for i, m := range models {
		if q == "" || strings.Contains(strings.ToLower(m.ID), q) || strings.Contains(strings.ToLower(m.Name), q) {
			out = append(out, i)
		}
	}
	return out
}

// findModel returns the index of the model with this ID, or -1.
func findModel(models []provider.ModelInfo, id string) int {
	for i, m := range models {
		if m.ID == id {
			return i
		}
	}
	return -1
}

// savedModelNote says whether the model switched to was saved as the
// provider's, for the line that reports the switch.
func savedModelNote(opts *ChatOptions, conn *setup.Conn) (text string, k ink) {
	if opts.Providers == nil {
		return ", for this session (there is no config file to save it in)", inkGray
	}
	if err := opts.Providers.SetModel(opts.Status.Provider, conn); err != nil {
		return ", for this session; not saved: " + err.Error(), inkYellow
	}
	return ", saved for " + opts.Status.Provider, inkGray
}

// The fullscreen UI.

// modelCommand runs /model: it opens the list of models, which fills in
// when the server has answered, or switches at once to a model named in
// full.
func (f *fullscreen) modelCommand(arg string) {
	if f.opts.Models == nil || f.opts.SetModel == nil {
		f.say("This session can't switch models; /provider use switches providers.", inkYellow)
		return
	}
	if f.busy {
		f.flash("wait for the turn to end, or press esc")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &modelPanel{provider: f.opts.Status.Provider, now: f.opts.Status.Model, filter: []rune(arg), loading: true, cancel: cancel}
	f.openPanel(p)
	go func() {
		models, err := f.opts.Models(ctx)
		f.post(func() {
			if f.panel != p {
				return // closed meanwhile
			}
			p.loading = false
			switch {
			case err != nil && arg != "":
				// A server that can't list its models may still serve the one
				// named.
				f.closePanel()
				f.say("Can't list the models ("+errorText(err)+"); using "+arg+" anyway.", inkYellow)
				f.switchModel(arg)
			case err != nil:
				p.err = errorText(err)
			case arg != "" && findModel(models, arg) >= 0:
				f.closePanel()
				f.switchModel(arg)
			case len(models) == 0:
				p.err = f.opts.Status.Provider + " lists no models; /model <id> names one"
			default:
				p.models = models
			}
		})
	}()
}

// switchModel switches the session to a model of the provider in use, on
// another goroutine, and reports.
func (f *fullscreen) switchModel(model string) {
	if model == f.opts.Status.Model {
		f.say("Already using "+model+".", inkGray)
		return
	}
	if f.busy {
		f.flash("wait for the turn to end, or press esc")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.busy, f.cancel, f.turnStart, f.firstDelta, f.deltas, f.activity = true, cancel, time.Now(), time.Time{}, 0, "Switching to "+model
	go func() {
		conn, warning, err := f.opts.SetModel(ctx, model)
		f.post(func() {
			f.busy = false
			if f.cancel != nil {
				f.cancel()
				f.cancel = nil
			}
			switch {
			case errors.Is(err, context.Canceled):
				f.say("Interrupted; the model stays "+f.opts.Status.Model+".", inkYellow)
			case err != nil:
				f.say("error: "+errorText(err), inkRed)
			default:
				if warning != "" {
					f.say("warning: "+warning, inkYellow)
				}
				f.opts.Status.Model, f.opts.Status.ContextWindow, f.opts.EffortLevels = conn.Provider.Model, conn.ContextWindow, conn.EffortLevels
				f.lastUsage = nil
				f.refreshBanner()
				note, k := savedModelNote(&f.opts, conn)
				f.tr.add(&noteBlock{spans: []span{plain("Now using " + conn.Provider.Model), inked(note, k)}})
				f.scroll = 0
			}
			f.sendQueued()
		})
	}()
}

// modelPanel lists the models of the provider in use: ↑ ↓ choose, Enter
// switches, typing narrows the list, Esc leaves the model as it is.
type modelPanel struct {
	provider string
	now      string // the model in use
	models   []provider.ModelInfo
	loading  bool
	err      string
	cancel   context.CancelFunc // stops the listing
	filter   []rune
	sel      int   // the chosen one, among those matching the filter
	top      int   // the first one shown
	visible  int   // how many are shown at once
	owners   []int // the model each row drawn shows, or -1
	rowsTop  int   // the row the list starts at
}

func (p *modelPanel) matched() []int { return filterModels(p.models, string(p.filter)) }

// choose switches to the model chosen, or, when nothing listed matches
// what was typed, to what was typed.
func (p *modelPanel) choose(f *fullscreen) bool {
	matched := p.matched()
	switch {
	case p.loading:
		return false
	case len(matched) > 0:
		p.sel = max(0, min(p.sel, len(matched)-1))
		f.switchModel(p.models[matched[p.sel]].ID)
	case strings.TrimSpace(string(p.filter)) != "":
		f.switchModel(strings.TrimSpace(string(p.filter)))
	default:
		return false
	}
	p.cancel()
	return true
}

func (p *modelPanel) key(f *fullscreen, k keyPress) bool {
	matched := p.matched()
	n := len(matched)
	page := max(1, p.visible-1)
	switch {
	case k.k == keyEsc:
		p.cancel()
		return true
	case k.k == keyEnter:
		return p.choose(f)
	case k.k == keyUp, k.k == keyBackTab:
		p.sel--
	case k.k == keyDown, k.k == keyTab:
		p.sel++
	case k.k == keyPgUp:
		p.sel -= page
	case k.k == keyPgDn:
		p.sel += page
	case k.k == keyHome:
		p.sel = 0
	case k.k == keyEnd:
		p.sel = n - 1
	case k.k == keyBackspace:
		if len(p.filter) > 0 {
			p.filter = p.filter[:len(p.filter)-1]
			p.sel = 0
		}
	case k.ctrl('u'):
		p.filter, p.sel = nil, 0
	case k.k == keyRune && k.mod&(modCtrl|modAlt) == 0 && k.r > ' ':
		p.filter = append(p.filter, k.r)
		p.sel = 0
	}
	p.sel = max(0, min(p.sel, n-1))
	return false
}

// paste types pasted text into the filter, on one line.
func (p *modelPanel) paste(f *fullscreen, text string) {
	p.filter = append(p.filter, []rune(strings.Join(strings.Fields(text), ""))...)
	p.sel = 0
}

func (p *modelPanel) click(f *fullscreen, row int) bool {
	if row < 0 || row >= len(p.owners) || p.owners[row] < 0 {
		return false
	}
	matched := p.matched()
	for i, m := range matched {
		if m == p.owners[row] {
			p.sel = i
			return p.choose(f)
		}
	}
	return false
}

func (p *modelPanel) wheel(f *fullscreen, up bool) {
	if up {
		p.sel--
	} else {
		p.sel++
	}
	p.sel = max(0, min(p.sel, len(p.matched())-1))
}

func (p *modelPanel) rows(f *fullscreen, width int) (rows [][]span) {
	defer func() {
		for i := range rows {
			rows[i] = cut(rows[i], width)
		}
	}()
	p.owners = p.owners[:0]
	add := func(owner int, rs ...[]span) {
		for _, r := range rs {
			rows = append(rows, r)
			p.owners = append(p.owners, owner)
		}
	}
	sym := f.term.sym()
	add(-1, rule(width))
	title := []span{plain(" "), styled("Models of "+p.provider, look{bold: true})}
	if p.now != "" {
		title = append(title, grayed(" · now "+p.now))
	}
	add(-1, title)
	switch {
	case p.loading:
		add(-1, []span{plain(" "), styled(spinnerFrames[f.frame%len(spinnerFrames)]+" ", look{fg: inkCyan, bold: true}), grayed("Listing the models… (esc cancels)")})
		return rows
	case p.err != "":
		add(-1, wrap([]span{inked("error: "+p.err, inkRed)}, width, []span{plain(" ")}, []span{plain(" ")})...)
		add(-1, []span{grayed("   esc closes")})
		return rows
	}

	matched := p.matched()
	filter := strings.TrimSpace(string(p.filter))
	if len(matched) == 0 {
		add(-1, wrap([]span{inked(fmt.Sprintf("No listed model matches %q.", filter), inkYellow)}, width, []span{plain(" ")}, []span{plain(" ")})...)
		add(-1, []span{grayed("   enter uses it anyway · backspace edits · esc cancels")})
		return rows
	}
	// The list takes what the screen leaves for panels, at most two thirds
	// of it; the chosen model stays in sight.
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
	labelW := 0
	for _, i := range matched {
		label, _ := modelLabel(p.models[i])
		labelW = max(labelW, textWidth(label))
	}
	labelW = min(labelW, max(12, width/2))
	for j := p.top; j < len(matched) && j < p.top+p.visible; j++ {
		i := matched[j]
		label, detail := modelLabel(p.models[i])
		l, mark := look{}, "   "
		if j == p.sel {
			l, mark = look{fg: inkCyan, bold: true}, " "+sym.pointer+" "
		}
		label = clip(label, labelW)
		row := []span{styled(mark, l), styled(label, l), plain(strings.Repeat(" ", labelW-textWidth(label)+2))}
		if detail != "" {
			row = append(row, grayed(detail))
		}
		if p.models[i].ID == p.now {
			row = append(row, inked(" · now", inkCyan))
		}
		add(i, row)
	}
	cursor := styled(" ", look{reverse: true})
	if filter == "" {
		add(-1, []span{grayed("   type to filter "), cursor})
	} else {
		add(-1, []span{grayed("   filter: "), plain(string(p.filter)), cursor})
	}
	hints := "↑↓ choose · enter switches and saves it for " + p.provider + " · esc cancel"
	if len(matched) > p.visible {
		hints += fmt.Sprintf(" · %d of %d", p.sel+1, len(matched))
	}
	add(-1, []span{grayed("   " + clip(hints, max(1, width-4)))})
	return rows
}

// Line mode.

// modelCommand runs /model in line mode: alone, it lists the models to
// pick from; with an ID, it switches to that model.
func (c *chat) modelCommand(arg string) {
	if c.opts.Models == nil || c.opts.SetModel == nil {
		c.r.println(c.st.yellow("This session can't switch models; /provider use switches providers."))
		return
	}
	ctx, stop := c.Interruptible()
	defer stop()
	c.r.startSpinner("listing the models of " + c.opts.Status.Provider)
	models, err := c.opts.Models(ctx)
	c.r.stopSpinner()
	model := arg
	switch {
	case errors.Is(err, context.Canceled):
		c.r.println(c.st.yellow("Cancelled; the model stays " + c.opts.Status.Model + "."))
		return
	case err != nil && arg != "":
		c.r.println(c.st.yellow("Can't list the models (" + errorText(err) + "); using " + arg + " anyway."))
	case err != nil:
		c.r.println(c.st.red("error: " + errorText(err)))
		return
	case arg != "" && findModel(models, arg) >= 0:
	case len(models) == 0:
		c.r.println(c.st.yellow(c.opts.Status.Provider + " lists no models; /model <id> names one."))
		return
	default:
		matched := filterModels(models, arg)
		if len(matched) == 0 {
			c.r.println(c.st.yellow(fmt.Sprintf("No listed model matches %q; using it anyway.", arg)))
			break
		}
		ch := setup.Choice{Question: "Which model?", Other: "Another model, by its ID"}
		for j, i := range matched {
			label, detail := modelLabel(models[i])
			if models[i].ID == c.opts.Status.Model {
				detail = strings.TrimPrefix(detail+" · now", " · ")
				ch.Default = j
			}
			ch.Options = append(ch.Options, setup.Option{Label: label, Detail: detail, Key: models[i].ID})
		}
		i, typed, err := c.Choose(ch)
		if err != nil {
			c.r.println(c.st.yellow("Cancelled; the model stays " + c.opts.Status.Model + "."))
			return
		}
		model = typed
		if i >= 0 {
			model = models[matched[i]].ID
		}
	}
	if model == c.opts.Status.Model {
		c.r.println(c.st.gray("Already using " + model + "."))
		return
	}
	c.r.startSpinner("switching to " + model)
	conn, warning, err := c.opts.SetModel(ctx, model)
	c.r.stopSpinner()
	switch {
	case errors.Is(err, context.Canceled):
		c.r.println(c.st.yellow("Cancelled; the model stays " + c.opts.Status.Model + "."))
		return
	case err != nil:
		c.r.println(c.st.red("error: " + errorText(err)))
		return
	}
	if warning != "" {
		c.r.println(c.st.yellow("warning: " + warning))
	}
	c.opts.Status.Model, c.opts.Status.ContextWindow, c.opts.EffortLevels = conn.Provider.Model, conn.ContextWindow, conn.EffortLevels
	c.lastUsage = nil
	note, k := savedModelNote(&c.opts, conn)
	if k == inkYellow {
		note = c.st.yellow(note)
	} else {
		note = c.st.gray(note)
	}
	c.r.println("Now using " + conn.Provider.Model + note)
}
