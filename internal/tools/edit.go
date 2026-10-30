package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"debbie/internal/prompt"
)

// editSnippetContext is how many lines around a change the result shows.
const editSnippetContext = 4

// EditFile replaces text in a file (SPEC §8.2).
type EditFile struct{}

func (EditFile) Name() string        { return "edit" }
func (EditFile) Kind() Kind          { return Edit }
func (EditFile) Description() string { return prompt.Tool("edit") }

func (EditFile) Params() []Param {
	return []Param{
		pathParam,
		{Name: "old_string", Type: "string", Required: true, Description: "The exact text to replace; empty to create a new file."},
		{Name: "new_string", Type: "string", Required: true, Description: "The text to put in its place."},
		{Name: "replace_all", Type: "boolean", Description: "Replace every match instead of exactly one."},
	}
}

func (EditFile) Prepare(_ context.Context, env *Env, args Args) (*Call, error) {
	abs, inside, err := env.Workspace.ResolveForWrite(args.String("path"))
	if err != nil {
		return nil, err
	}
	shown := env.Workspace.Rel(abs)
	// The file's text has LF line endings; so must the strings.
	old := strings.ReplaceAll(args.String("old_string"), "\r\n", "\n")
	repl := strings.ReplaceAll(args.String("new_string"), "\r\n", "\n")

	var before []byte
	var f textFile
	var oldText, newText string
	switch _, statErr := os.Stat(abs); {
	case old == "" && errors.Is(statErr, fs.ErrNotExist):
		newText = repl // a new file
	case old == "" && statErr == nil:
		return nil, fmt.Errorf("%s already exists; to replace all of it use write_file, to change part of it give old_string", shown)
	case errors.Is(statErr, fs.ErrNotExist):
		return nil, fmt.Errorf("%s does not exist; to create it, use an empty old_string", shown)
	case statErr != nil:
		return nil, statErr
	default:
		if old == repl {
			return nil, errors.New("old_string and new_string are identical, so there is nothing to change")
		}
		if err := env.Files.CheckUnchanged(abs, shown); err != nil {
			return nil, err
		}
		if f, before, err = readText(abs, shown); err != nil {
			return nil, err
		}
		oldText = f.text
		matches, err := matchEdit(oldText, old, repl, args.Bool("replace_all"))
		if errors.Is(err, errNoMatch) {
			return nil, fmt.Errorf("old_string was not found in %s, not even ignoring trailing whitespace and typographic quotes and dashes. "+
				"Check its whitespace, indentation and escaping, and copy it from the file without line numbers; read the file again if it may have changed.%s",
				shown, closestLines(oldText, old))
		}
		if err != nil {
			return nil, err
		}
		if repl == "" && !strings.HasSuffix(old, "\n") {
			// Deleting whole lines takes their newline too, leaving no blank line.
			for i, m := range matches {
				if (m.start == 0 || oldText[m.start-1] == '\n') && m.end < len(oldText) && oldText[m.end] == '\n' {
					matches[i].end++
				}
			}
		}
		newText = applyMatches(oldText, matches)
		if newText == oldText {
			return nil, errors.New("the edit would leave the file unchanged")
		}
	}

	diff := UnifiedDiff(shown, shown, oldText, newText)
	if before == nil {
		diff = UnifiedDiff("/dev/null", shown, "", newText)
	}
	added, removed := DiffStats(diff)
	call := &Call{
		Kind:       Edit,
		Summary:    fmt.Sprintf("edit %s (+%d -%d)", shown, added, removed),
		Preview:    diff,
		SessionKey: "edit:" + trackKey(abs),
	}
	if !inside {
		outsideWrite(call, abs)
	}
	data := f.encode(newText)
	call.Run = func(context.Context) (Result, error) {
		if err := unchangedSince(abs, shown, before); err != nil {
			return Result{}, err
		}
		if err := writeFile(abs, data); err != nil {
			return Result{}, err
		}
		env.Files.Record(abs, data)
		verb := "updated"
		if before == nil {
			verb = "created"
		}
		return Result{
			Output:  fmt.Sprintf("%s was %s. %s", shown, verb, snippet(oldText, newText)),
			Display: fmt.Sprintf("+%d -%d", added, removed),
		}, nil
	}
	return call, nil
}

// snippet shows the changed lines of the new text with a few lines around
// them, so the model can check the result without reading the file again.
func snippet(oldText, newText string) string {
	a := strings.Split(oldText, "\n")
	b := strings.Split(newText, "\n")
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	total := countLines(newText)
	start := max(0, pre-editSnippetContext)
	end := min(len(b), len(b)-suf+editSnippetContext)
	if end-start > 400 {
		return fmt.Sprintf("The change spans %d lines; read the file to see them.", end-start)
	}
	end = min(end, total)
	if end <= start {
		return fmt.Sprintf("The file now has %s.", Lines(total))
	}
	return fmt.Sprintf("Lines %d-%d of %d now read:\n%s", start+1, end, total, strings.Join(b[start:end], "\n"))
}
