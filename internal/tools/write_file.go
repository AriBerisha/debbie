package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"debbie/internal/prompt"
)

// WriteFile creates a file or replaces its content.
type WriteFile struct{}

func (WriteFile) Name() string        { return "write_file" }
func (WriteFile) Kind() Kind          { return Edit }
func (WriteFile) Description() string { return prompt.Tool("write_file") }

func (WriteFile) Params() []Param {
	return []Param{
		pathParam,
		{Name: "content", Type: "string", Required: true, Description: "The complete content of the file."},
	}
}

func (WriteFile) Prepare(_ context.Context, env *Env, args Args) (*Call, error) {
	abs, inside, err := env.Workspace.ResolveForWrite(args.String("path"))
	if err != nil {
		return nil, err
	}
	shown := env.Workspace.Rel(abs)
	content := args.String("content")

	var before []byte // nil when the file is new
	f := textFile{}
	if fi, err := os.Stat(abs); err == nil {
		if fi.IsDir() {
			return nil, fmt.Errorf("%s is a directory", shown)
		}
		if err := env.Files.CheckUnchanged(abs, shown); err != nil {
			return nil, err
		}
		var old textFile
		if old, before, err = readText(abs, shown); err != nil {
			return nil, err
		}
		f = textFile{bom: old.bom, crlf: old.crlf} // keep the file's line endings and BOM
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	text := decodeText([]byte(content)).text
	after := f.encode(text)

	call := &Call{Kind: Edit, SessionKey: "edit:" + trackKey(abs)}
	if !inside {
		outsideWrite(call, abs)
	}
	if before == nil {
		call.Summary = fmt.Sprintf("write_file %s (new, %s)", shown, Lines(countLines(text)))
		call.Preview = UnifiedDiff("/dev/null", shown, "", text)
	} else {
		diff := UnifiedDiff(shown, shown, decodeText(before).text, text)
		added, removed := DiffStats(diff)
		call.Summary = fmt.Sprintf("write_file %s (+%d -%d)", shown, added, removed)
		call.Preview = diff
	}
	call.Run = func(context.Context) (Result, error) {
		if err := unchangedSince(abs, shown, before); err != nil {
			return Result{}, err
		}
		if err := writeFile(abs, after); err != nil {
			return Result{}, err
		}
		env.Files.Record(abs, after)
		if before == nil {
			return Result{Output: fmt.Sprintf("Created %s (%s).", shown, Lines(countLines(text))), Display: "created"}, nil
		}
		return Result{Output: fmt.Sprintf("Wrote %s (%s).", shown, Lines(countLines(text))), Display: "written"}, nil
	}
	return call, nil
}

// unchangedSince checks, just before writing, that the file still holds
// what it held when the change was prepared (nil: it didn't exist), in case
// it changed while the user looked at the approval prompt.
func unchangedSince(abs, shown string, before []byte) error {
	now, err := os.ReadFile(abs)
	switch {
	case before == nil && errors.Is(err, fs.ErrNotExist):
		return nil
	case before == nil && err == nil:
		return fmt.Errorf("%s was created by someone else in the meantime; read it before writing", shown)
	case err != nil:
		return fmt.Errorf("%s changed in the meantime: %w", shown, err)
	case !bytes.Equal(now, before):
		return fmt.Errorf("%s changed in the meantime; read it again", shown)
	}
	return nil
}
