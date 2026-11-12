package tools

import (
	"context"
	"fmt"
	"strings"

	"debbie/internal/prompt"
)

const (
	readDefaultLines = 2000
	readMaxLineChars = 2000
)

var pathParam = Param{Name: "path", Type: "string", Required: true,
	Description: "The file, relative to the workspace root, or absolute.",
	Aliases:     []string{"file_path", "absolute_path", "filename", "file"}}

// ReadFile returns a text file's lines, numbered.
type ReadFile struct{}

func (ReadFile) Name() string        { return "read_file" }
func (ReadFile) Kind() Kind          { return Read }
func (ReadFile) Description() string { return prompt.Tool("read_file") }

func (ReadFile) Params() []Param {
	return []Param{
		pathParam,
		{Name: "offset", Type: "integer", Description: "The line to start at, counting from 1."},
		{Name: "limit", Type: "integer", Description: fmt.Sprintf("How many lines to return; %d by default.", readDefaultLines)},
	}
}

func (ReadFile) Prepare(_ context.Context, env *Env, args Args) (*Call, error) {
	abs, inside, err := env.Workspace.Resolve(args.String("path"))
	if err != nil {
		return nil, err
	}
	shown := env.Workspace.Rel(abs)
	offset := max(args.Int("offset", 1), 1)
	limit := args.Int("limit", readDefaultLines)
	if limit <= 0 {
		limit = readDefaultLines
	}
	summary := "read_file " + shown
	if args.Has("offset") || args.Has("limit") {
		summary += fmt.Sprintf(" (from line %d)", offset)
	}
	call := &Call{Kind: Read, Summary: summary}
	if !inside {
		call.Outside, call.SessionKey = []string{abs}, OutsideReadKey
	}
	call.Run = func(context.Context) (Result, error) {
		if res, ok, err := readImage(abs, shown); ok {
			return res, err
		}
		f, data, err := readText(abs, shown)
		if err != nil {
			return Result{}, err
		}
		env.Files.Record(abs, data)
		if f.text == "" {
			return Result{Output: shown + " is empty.", Display: "empty"}, nil
		}
		lines := strings.Split(strings.TrimSuffix(f.text, "\n"), "\n")
		if offset > len(lines) {
			return Result{}, fmt.Errorf("%s has only %s; offset %d is past the end", shown, Lines(len(lines)), offset)
		}
		end := min(len(lines), offset-1+limit)
		var b strings.Builder
		if offset > 1 || end < len(lines) {
			fmt.Fprintf(&b, "Showing lines %d-%d of %d. Use offset and limit to read more.\n\n", offset, end, len(lines))
		}
		for i := offset - 1; i < end; i++ {
			line := lines[i]
			if len(line) > readMaxLineChars {
				line = firstRunes(line, readMaxLineChars) + " … [line cut]"
			}
			fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
		}
		return Result{Output: b.String(), Display: Lines(end - offset + 1)}, nil
	}
	return call, nil
}
