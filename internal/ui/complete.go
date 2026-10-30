package ui

import (
	"fmt"
	"strings"
	"unicode"

	"debbie/internal/tools"
)

// Completion above the input box, like Claude Code's: while the input holds
// the start of a command, the commands it may be (MCP prompts among them);
// after an @, workspace paths, or an MCP server's resources after
// @<server>:. ↑ ↓ choose, Tab or Enter accepts, so does a click.

// suggestion is one completion offered.
type suggestion struct {
	text  string // what replaces the word being completed
	label string
	about string
	more  bool // accepting it keeps completing, as for a directory
	args  bool // a command that takes arguments
}

// maxSuggestions is how many completions are listed at once.
const maxSuggestions = 8

// wordEnd reports whether r ends a word to complete: a space, or a paste's
// chip.
func wordEnd(r rune) bool { return strings.ContainsRune(" \n\t", r) || inChipArea(r) }

// suggestions returns the completions for what is being typed, and where
// the word they replace starts.
func (f *fullscreen) suggestions() (list []suggestion, start int) {
	text := f.ed.text
	cur := f.ed.cursor
	if len(text) > 0 && text[0] == '/' && !strings.ContainsRune(string(text), '\n') && cur == len(text) {
		if list, ok := f.commandSuggestions(string(text)); ok {
			return list, 0
		}
	}
	start = cur
	for start > 0 && !wordEnd(text[start-1]) {
		start--
	}
	word := string(text[start:cur])
	if !strings.HasPrefix(word, "@") || cur < len(text) && !wordEnd(text[cur]) {
		return nil, 0
	}
	query := word[1:]
	key := query
	if f.suggCache.key == key && f.suggCache.valid {
		return f.suggCache.list, start
	}
	if server, rest, ok := strings.Cut(query, ":"); ok && f.opts.MCP != nil {
		for _, r := range f.opts.MCP.Resources() {
			if r.Server == server && strings.Contains(strings.ToLower(r.URI+" "+r.Name), strings.ToLower(rest)) {
				list = append(list, suggestion{text: "@" + r.Server + ":" + r.URI, label: r.Server + ":" + r.URI, about: firstLine(r.Name+" "+r.Description, 60)})
				if len(list) == maxSuggestions {
					break
				}
			}
		}
	} else {
		if f.opts.Files != nil {
			for _, p := range f.opts.Files.Complete(query, maxSuggestions) {
				list = append(list, suggestion{text: "@" + p, label: p, more: strings.HasSuffix(p, "/")})
			}
		}
		if f.opts.MCP != nil && len(list) < maxSuggestions {
			seen := map[string]bool{}
			for _, r := range f.opts.MCP.Resources() {
				if !seen[r.Server] && strings.HasPrefix(r.Server, query) {
					seen[r.Server] = true
					list = append(list, suggestion{text: "@" + r.Server + ":", label: r.Server + ":", about: "resources of the MCP server " + r.Server, more: true})
				}
			}
		}
	}
	f.suggCache = suggestCache{key: key, list: list, valid: true}
	return list, start
}

// commandSuggestions completes a command being typed: the commands it
// starts; after a command's name, or with it typed in full, its
// subcommands; and the commands and subcommands whose description
// mentions a word that starts with what was typed, so /chrome finds
// /browser install. ok is false past the subcommand.
func (f *fullscreen) commandSuggestions(typed string) (list []suggestion, ok bool) {
	seen := map[string]bool{}
	add := func(s suggestion) {
		if !seen[s.text] && s.text != typed && len(list) < maxSuggestions {
			seen[s.text] = true
			list = append(list, s)
		}
	}
	name, rest, spaced := strings.Cut(typed, " ")
	if spaced {
		// Past the command's name, only its subcommands; anything else,
		// such as an @ mention, completes as in a message.
		c := findCommand(name)
		if c == nil || strings.Contains(rest, " ") {
			return nil, false
		}
		for _, s := range c.subs {
			if strings.HasPrefix(s.name, rest) {
				add(subSuggestion(c, s))
			}
		}
		return list, len(list) > 0
	}
	for _, c := range f.commands() {
		if strings.HasPrefix(c.text, typed) {
			add(c)
		}
	}
	if c := findCommand(typed); c != nil {
		for _, s := range c.subs {
			add(subSuggestion(c, s))
		}
	}
	if q := strings.ToLower(typed[1:]); len(q) >= 2 {
		for i := range commandList {
			c := &commandList[i]
			found := mentions(c.about+" "+c.words, q)
			if found {
				add(suggestion{text: c.name, label: c.name, about: c.about, args: c.args != ""})
			}
			for _, s := range c.subs {
				if found || strings.HasPrefix(s.name, q) || mentions(s.about, q) {
					add(subSuggestion(c, s))
				}
			}
		}
	}
	return list, true
}

func findCommand(name string) *command {
	for i := range commandList {
		if commandList[i].name == name {
			return &commandList[i]
		}
	}
	return nil
}

func subSuggestion(c *command, s subcommand) suggestion {
	text := c.name + " " + s.name
	return suggestion{text: text, label: text, about: s.about, args: s.args}
}

// mentions reports whether text has a word that starts with q.
func mentions(text, q string) bool {
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if strings.HasPrefix(w, q) {
			return true
		}
	}
	return false
}

// suggestCache keeps the last path completion, so the workspace is walked
// once per change of the word, not once per frame.
type suggestCache struct {
	key   string
	list  []suggestion
	valid bool
}

// commands are the slash commands: the built-in ones, then the prompts of
// the MCP servers, as /mcp__<server>__<prompt>.
func (f *fullscreen) commands() []suggestion {
	var out []suggestion
	for _, c := range commandList {
		out = append(out, suggestion{text: c.name, label: c.name, about: c.about, args: c.args != ""})
	}
	if f.opts.MCP != nil {
		for _, p := range f.opts.MCP.Prompts() {
			about := p.Title
			if p.Description != "" {
				about = p.Description
			}
			out = append(out, suggestion{text: p.Command, label: p.Command, about: firstLine(about, 80) + " (MCP)", args: len(p.Arguments) > 0})
		}
	}
	return out
}

func (f *fullscreen) completionRows(list []suggestion, width int) [][]span {
	labelW := 0
	for _, s := range list {
		labelW = max(labelW, textWidth(s.label))
	}
	labelW = min(labelW, max(12, width/2))
	var rows [][]span
	for i, s := range list {
		l := look{fg: inkGray}
		mark := "  "
		if i == f.completion {
			l = look{fg: inkCyan, bold: true}
			mark = f.term.sym().pointer + " "
		}
		label := clip(s.label, labelW)
		row := []span{styled(mark, l), styled(label, l)}
		if s.about != "" {
			row = append(row, plain(strings.Repeat(" ", labelW-textWidth(label)+2)), grayed(s.about))
		}
		rows = append(rows, cut(row, width))
	}
	return rows
}

// completionKey moves through and accepts completions: ↑ ↓ choose, Tab or
// Enter on a chosen one accepts it.
func (f *fullscreen) completionKey(p keyPress) bool {
	list, start := f.suggestions()
	if len(list) == 0 {
		return false
	}
	switch {
	case p.k == keyUp:
		if f.completion <= 0 {
			f.completion = len(list) - 1
		} else {
			f.completion--
		}
	case p.k == keyDown:
		f.completion = (f.completion + 1) % len(list)
	case p.k == keyTab:
		if f.completion < 0 {
			f.completion = 0
		}
		f.acceptCompletion(list, start)
	case p.k == keyEnter && p.mod == 0 && f.completion >= 0:
		f.acceptCompletion(list, start)
	default:
		return false
	}
	return true
}

// acceptCompletion puts the chosen completion in place of the word being
// completed.
func (f *fullscreen) acceptCompletion(list []suggestion, start int) {
	if f.completion < 0 || f.completion >= len(list) {
		return
	}
	s := list[f.completion]
	e := f.ed
	tail := append([]rune(nil), e.text[e.cursor:]...)
	e.text = append(append(e.text[:start:start], []rune(s.text)...), tail...)
	e.cursor, e.column = start+len([]rune(s.text)), -1
	if !s.more && (s.args || strings.HasPrefix(s.text, "@")) && (len(tail) == 0 || tail[0] != ' ') {
		e.insert(" ")
	}
	f.completion = -1
	f.suggCache.valid = false
}

// The @ mentions of a message: files of the workspace and resources of MCP
// servers, attached to what the model gets.

// attached says what an @ mention attached, or why it didn't, under the
// message.
type attached struct {
	text string
	ok   bool
}

// expandMentions attaches the files and resources a message mentions:
// attach reads a workspace file, resource an MCP server's resource, for the
// servers named in servers. A mention that names nothing is left alone, as
// @ also starts names.
func expandMentions(text string, attach func(string) (tools.Attachment, error),
	resource func(server, uri string) (string, error), servers map[string]bool) (string, []attached) {
	var blocks []string
	var notes []attached
	seen := map[string]bool{}
	for _, word := range strings.Fields(text) {
		if !strings.HasPrefix(word, "@") || len(word) < 2 {
			continue
		}
		name := strings.TrimRight(word[1:], ".,;!?)]}\"'")
		if seen[name] {
			continue
		}
		seen[name] = true
		if server, uri, ok := strings.Cut(name, ":"); ok && servers[server] && resource != nil {
			content, err := resource(server, uri)
			if err != nil {
				notes = append(notes, attached{text: "not attached: " + name + ": " + err.Error()})
				continue
			}
			blocks = append(blocks, content)
			n := strings.Count(content, "\n") - 1
			notes = append(notes, attached{text: fmt.Sprintf("attached %s · %d line%s", name, n, pluralS(n)), ok: true})
			continue
		}
		if attach == nil {
			continue
		}
		a, err := attach(name)
		if err != nil {
			if strings.ContainsAny(name, "/.") { // it looked like a path
				notes = append(notes, attached{text: "not attached: " + err.Error()})
			}
			continue
		}
		blocks = append(blocks, fmt.Sprintf("<attached_file path=%q lines=\"%d\">\n%s\n</attached_file>", a.Path, a.Lines, a.Text))
		notes = append(notes, attached{text: fmt.Sprintf("attached %s · %d line%s", a.Path, a.Lines, pluralS(a.Lines)), ok: true})
	}
	if len(blocks) == 0 {
		return text, notes
	}
	return text + "\n\n" + strings.Join(blocks, "\n\n"), notes
}
