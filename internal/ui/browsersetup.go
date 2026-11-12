package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"debbie/internal/browser"
	"debbie/internal/config"
)

// The steps only the user can take: Chrome allows loading an unpacked
// extension by hand only.
const chromeExtensionsPage = "chrome://extensions"

// chromeStep is one step in Chrome: its title, how to take it, and what it
// pastes ("page", "folder" or nothing), which the panel puts on the
// clipboard when the step comes.
type chromeStep struct {
	title, how, paste string
}

// chromeSteps are the steps in Chrome, in the order the user takes them,
// which is the order they paste things in: the page's address, then the
// folder's path. reload is for a Chrome that runs an older copy, which
// only needs a reload. pasting says the clipboard holds what each step
// pastes; in line mode, or when copying failed, it doesn't.
func chromeSteps(reload, pasting bool) []chromeStep {
	open := chromeStep{title: "Open " + chromeExtensionsPage, paste: "page",
		how: "Type it into Chrome's address bar and press Enter."}
	if pasting {
		open.how = "It's on your clipboard: paste it into Chrome's address bar and press Enter."
	}
	if reload {
		return []chromeStep{open, {title: "Reload the extension",
			how: "On the card of " + browser.ExtensionName() + ", click the reload button, the round arrow."}}
	}
	load := chromeStep{title: "Load the extension", paste: "folder", how: "Click Load unpacked and choose this folder:"}
	if pasting {
		load.how = "Click Load unpacked, paste the folder's path (now on your clipboard) into the folder picker's " +
			"address bar, and click Select Folder:"
	}
	return []chromeStep{open, {title: "Turn on Developer mode", how: "Its switch is at the top right of the extensions page."}, load}
}

// reloadHint is for a user who loaded the extension before.
const reloadHint = "Loaded it before? Then open " + chromeExtensionsPage + " and click the reload button on its card instead."

// replaceNote is for a user who loaded the copy a former name of the
// program installed: the new folder takes its place.
func replaceNote(former string) string {
	return "Chrome replaces the copy you loaded before, from " + former + ", with this one: it is the same extension, renamed."
}

// profileNote says what the extension can do in the user's own profile.
const profileNote = "It works in your Chrome profile, signed in as you: on the sites it allows, the agent reads and clicks " +
	"as you would, and anything that would buy, subscribe, post or sign in asks you first."

// browserPanel walks the user through /web browser install one step at a
// time: what the install did, then the steps in Chrome with the one at
// hand highlighted and what it pastes on the clipboard, and whether
// Chrome connected.
type browserPanel struct {
	installed bool
	err       error
	got       *browser.Installed
	where     string // where the host is registered
	webNote   string // what happened to web access
	step      int    // the step at hand, from 0
	reload    bool   // Chrome runs an older copy: the steps reload it
	noClip    bool   // copying failed: the steps don't count on the clipboard
	copied    string // what the last copy did
	sel       int
}

// browserInstall runs /web browser install in the fullscreen UI.
func (f *fullscreen) browserInstall() {
	w := f.opts.Web
	if w == nil {
		f.say("The browser needs the tools, which this chat doesn't have.", inkYellow)
		return
	}
	p := &browserPanel{}
	f.openPanel(p)
	go func() {
		got, in, err := w.Install()
		f.post(func() {
			p.installed, p.got, p.err = true, got, err
			if err != nil {
				return
			}
			p.where = in.Where
			p.webNote = webForChrome(&f.opts)
			p.goTo(f, 0) // the first step pastes the page's address
		})
	}()
}

// webForChrome turns web access on, safe, if it is off, so Chrome can
// connect, and says how it stands.
func webForChrome(opts *ChatOptions) string {
	w := opts.Web
	if mode := w.Mode(); mode != config.WebOff {
		return "Web access is " + mode + " in this session, so Chrome can connect"
	}
	if note := w.SetMode(config.WebSafe); note != "" {
		return "Turned web access on (safe), but " + note
	}
	return "Turned web access on (safe) in this session, so Chrome can connect; /web off turns it off"
}

// steps are the steps in Chrome as they stand. Once an older copy of the
// extension connects, they are the reload's, also while it reconnects.
func (p *browserPanel) steps(f *fullscreen) []chromeStep {
	if st := f.opts.Web.Bridge.Status(); !p.reload && st.Connected && st.Extension.Version != "" && !st.Extension.Current() {
		p.reload = true
		p.step = min(p.step, 1) // past opening the page, the reload is next
	}
	return chromeSteps(p.reload, !p.noClip)
}

// goTo moves to a step and puts what it pastes on the clipboard.
func (p *browserPanel) goTo(f *fullscreen, step int) {
	steps := p.steps(f)
	p.step, p.sel = max(0, min(step, len(steps)-1)), 0
	p.copy(f, steps[p.step].paste)
}

// copy puts the page's address or the folder's path on the clipboard and
// notes it.
func (p *browserPanel) copy(f *fullscreen, which string) {
	text, what := chromeExtensionsPage, chromeExtensionsPage
	switch which {
	case "folder":
		text, what = p.got.Extension, "the folder's path"
	case "page":
	default:
		return
	}
	via, err := f.clipboard(text)
	if err != nil {
		p.copied, p.noClip = "Couldn't copy "+what+": "+err.Error(), true
		return
	}
	p.copied, p.noClip = "Copied "+what+" to "+via, false
}

func (p *browserPanel) options(f *fullscreen) []string {
	if p.connected(f) {
		return []string{"Done"}
	}
	if !p.installed || p.err != nil {
		return []string{"Close"}
	}
	steps := p.steps(f)
	var options []string
	if p.step < len(steps)-1 {
		options = append(options, "Next")
	}
	switch steps[p.step].paste {
	case "page":
		options = append(options, "Copy "+chromeExtensionsPage+" again")
	case "folder":
		options = append(options, "Copy the folder's path again")
	}
	if p.step > 0 {
		options = append(options, "Back")
	}
	return append(options, "Close; Chrome can connect later")
}

// connected reports whether Chrome runs this binary's extension and is
// connected.
func (p *browserPanel) connected(f *fullscreen) bool {
	st := f.opts.Web.Bridge.Status()
	return p.installed && p.err == nil && st.Connected && st.Extension.Current()
}

// rows draws the panel. When the screen is too short for all of it, the
// least needed rows go first: the notes, then what the install did, and
// the step at hand and the options stay (fit).
func (p *browserPanel) rows(f *fullscreen, width int) [][]span {
	sym := f.term.sym()
	spin := spinnerFrames[f.frame%len(spinnerFrames)]
	var parts []part
	add := func(keep int, rows ...[]span) { parts = append(parts, part{keep, rows}) }
	line := func(mark string, k ink, text string) [][]span {
		return wrap([]span{inked(mark+" ", k), plain(text)}, width, []span{plain(" ")}, []span{plain("   ")})
	}
	note := func(text string) [][]span {
		return wrap([]span{grayed(text)}, width, []span{plain("   ")}, []span{plain("   ")})
	}
	options := optionRows(f, p.options(f), p.sel, width)
	title := []span{plain(" "), bolded("Set up the Chrome extension", ink{})}
	if p.installed && p.err == nil && !p.connected(f) {
		title = append(title, grayed(fmt.Sprintf(" · step %d of %d", p.step+1, len(p.steps(f)))))
	}
	add(4, rule(width))
	add(8, title)
	switch {
	case !p.installed:
		add(9, line(spin, inkCyan, "Writing the extension and registering it with Chrome…")...)
		add(10, options...)
		return fit(parts, f.panelLimit)
	case p.err != nil:
		add(9, line(sym.cross, inkRed, "The install failed: "+p.err.Error())...)
		add(10, options...)
		return fit(parts, f.panelLimit)
	}
	add(2, line(sym.check, inkGreen, "Wrote the extension and registered it with Chrome ("+p.where+")")...)
	add(2, line(sym.check, inkGreen, p.webNote)...)

	st := f.opts.Web.Bridge.Status()
	if p.connected(f) {
		add(9, line(sym.check, inkGreen, fmt.Sprintf("%s is connected: extension %s, ID %s. It may open %s.",
			st.Extension.Label(), st.Extension.Build, p.got.ID, siteNames(st.Extension.Sites)))...)
		add(1, note("Try: open youtube.com and search for something. Click the extension's icon to name "+
			"this browser; its options page (Details, then Extension options) adds sites.")...)
	} else {
		if p.reload {
			add(5, line("!", inkYellow, "Chrome runs an older copy of the extension, so it only needs a reload:")...)
		} else {
			add(3, wrap([]span{plain("Now load it in Chrome, which only you can do:")}, width, []span{plain(" ")}, []span{plain(" ")})...)
		}
		indent := []span{plain("     ")}
		for i, s := range p.steps(f) {
			switch {
			case i < p.step:
				add(7, wrap([]span{inked(" "+sym.check+" ", inkGreen), grayed(fmt.Sprintf("%d. %s", i+1, s.title))}, width, nil, indent)...)
			case i == p.step:
				now := wrap([]span{inked(" "+sym.pointer+" ", inkCyan), bolded(fmt.Sprintf("%d. %s", i+1, s.title), inkCyan)}, width, nil, indent)
				now = append(now, wrap([]span{plain(s.how)}, width, indent, indent)...)
				if s.paste == "folder" {
					now = append(now, wrap([]span{plain(p.got.Extension)}, width, []span{plain("       ")}, []span{plain("       ")})...)
				}
				add(10, now...)
			default:
				add(7, wrap([]span{grayed(fmt.Sprintf("   %d. %s", i+1, s.title))}, width, nil, indent)...)
			}
		}
		switch {
		case p.got.Former != "":
			add(5, note(replaceNote(p.got.Former))...)
		case !p.reload:
			add(0, note(reloadHint)...)
		}
		if st.Started { // if not, the note above says why: another session holds the browser
			add(6, line(spin, inkCyan, "Waiting for Chrome to connect…")...)
		}
		if p.copied != "" {
			add(1, note(p.copied)...)
		}
	}
	add(0, wrap([]span{grayed(profileNote)}, width, []span{plain(" ")}, []span{plain(" ")})...)
	add(10, options...)
	return fit(parts, f.panelLimit)
}

// part is a run of a panel's rows and how much they are needed: when the
// screen is short, fit leaves out the least needed first.
type part struct {
	keep int
	rows [][]span
}

// fit joins parts into at most limit rows, leaving out whole parts, the
// least needed first and of those the lowest first; what is needed most
// stays even when it alone is too tall, for draw to cut.
func fit(parts []part, limit int) [][]span {
	total := 0
	for _, pt := range parts {
		total += len(pt.rows)
	}
	for total > limit {
		drop := -1
		for i, pt := range parts {
			if len(pt.rows) > 0 && pt.keep < 10 && (drop < 0 || pt.keep <= parts[drop].keep) {
				drop = i
			}
		}
		if drop < 0 {
			break
		}
		total -= len(parts[drop].rows)
		parts[drop].rows = nil
	}
	var rows [][]span
	for _, pt := range parts {
		rows = append(rows, pt.rows...)
	}
	return rows
}

func (p *browserPanel) key(f *fullscreen, k keyPress) bool {
	options := p.options(f)
	p.sel = min(p.sel, len(options)-1)
	i := choiceKey(k, &p.sel, len(options))
	if i == -2 {
		return true // esc
	}
	return p.choose(f, i)
}

func (p *browserPanel) click(f *fullscreen, row int) bool {
	options := p.options(f)
	i := row - (len(p.rows(f, f.width)) - len(options))
	if i < 0 || i >= len(options) {
		return false
	}
	p.sel = i
	return p.choose(f, i)
}

// choose acts on an option, reporting whether the panel is done.
func (p *browserPanel) choose(f *fullscreen, i int) bool {
	options := p.options(f)
	if i < 0 || i >= len(options) {
		return false
	}
	switch o := options[i]; {
	case o == "Next":
		p.goTo(f, p.step+1)
	case o == "Back":
		p.goTo(f, p.step-1)
	case strings.HasPrefix(o, "Copy "):
		p.copy(f, p.steps(f)[p.step].paste)
		p.sel = 0
	default:
		return true
	}
	return false
}

// siteNames lists sites for a sentence.
func siteNames(sites []string) string {
	if len(sites) == 0 {
		return "no sites yet"
	}
	return strings.Join(sites, ", ")
}

// watchChrome flashes when Chrome connects or goes, and when it asks the
// user whether the agent may open a site, so the user learns of it with
// no panel open.
func (f *fullscreen) watchChrome() {
	if f.opts.Web == nil {
		return
	}
	st := f.opts.Web.Bridge.Status()
	if st.Asking != f.chromeAsking {
		f.chromeAsking = st.Asking
		if st.Asking != "" {
			f.flash(st.Extension.Label() + " is asking you whether the agent may open " + st.Asking + ": answer in the window it opened")
		}
	}
	now := st.Connected && st.Extension.Version != ""
	switch {
	case now == f.chromeShown:
		return
	case now && !st.Extension.Current():
		f.flash(st.Extension.Label() + " connected with an older copy of the extension: reload it in " + chromeExtensionsPage)
	case now:
		f.flash(st.Extension.Label() + " connected")
	default:
		f.flash("Chrome disconnected")
	}
	f.chromeShown = now
}

// browserInstall runs /web browser install in line mode: the install, then the
// steps in Chrome, then a wait for the connection that Ctrl+C ends.
func (c *chat) browserInstall() {
	w := c.opts.Web
	if w == nil {
		c.r.println(c.st.yellow("The browser needs the tools, which this chat doesn't have."))
		return
	}
	c.r.println("Setting up the Chrome extension.")
	got, in, err := w.Install()
	if err != nil {
		c.r.println(c.st.yellow("The install failed: " + err.Error()))
		return
	}
	c.r.println("✓ Wrote the extension and registered it with Chrome (" + in.Where + ").")
	c.r.println("✓ " + webForChrome(&c.opts) + ".")
	c.r.println("Now load it in Chrome, which only you can do:")
	for i, s := range chromeSteps(false, false) {
		c.r.println(fmt.Sprintf("  %d. %s", i+1, s.title))
		c.r.println("     " + s.how)
		if s.paste == "folder" {
			c.r.println("       " + got.Extension)
		}
	}
	if got.Former != "" {
		c.r.println("  " + replaceNote(got.Former))
	} else {
		c.r.println("  " + reloadHint)
	}
	c.r.println(c.st.gray(profileNote))
	ctx, stop := c.Interruptible()
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	c.r.println(c.st.gray("Waiting for Chrome to connect; Ctrl+C stops waiting."))
	stale := false
	for {
		st := w.Bridge.Status()
		switch {
		case !st.Started:
			return
		case st.Connected && st.Extension.Version != "" && st.Extension.Current():
			c.r.println(fmt.Sprintf("✓ %s is connected: extension %s, ID %s. It may open %s.", st.Extension.Label(), st.Extension.Build, got.ID, siteNames(st.Extension.Sites)))
			return
		case st.Connected && st.Extension.Version != "" && !stale:
			stale = true
			c.r.println(c.st.yellow("Chrome runs an older copy of the extension: open " + chromeExtensionsPage +
				" and click the reload button on the card of " + browser.ExtensionName() + "."))
		}
		select {
		case <-ctx.Done():
			c.r.println("Stopped waiting. /web browser shows whether Chrome connected.")
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}
