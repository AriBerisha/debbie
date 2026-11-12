package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"debbie/internal/prompt"
)

const (
	grepMaxMatches   = 200
	grepMaxLineChars = 300
)

// Grep searches file contents with a regular expression.
type Grep struct{}

func (Grep) Name() string        { return "grep" }
func (Grep) Kind() Kind          { return Read }
func (Grep) Description() string { return prompt.Tool("grep") }

func (Grep) Params() []Param {
	return []Param{
		{Name: "pattern", Type: "string", Required: true, Description: "A regular expression (RE2 syntax).",
			Aliases: []string{"regex", "query"}},
		{Name: "path", Type: "string", Description: "A directory or file to search; the workspace root when omitted.",
			Aliases: []string{"dir_path", "directory"}},
		{Name: "include", Type: "string", Description: "Only search paths matching this glob, such as *.go.", Aliases: []string{"glob"}},
		{Name: "exclude", Type: "string", Description: "Skip paths matching this glob."},
		{Name: "case_sensitive", Type: "boolean", Description: "Match letter case exactly; off by default."},
	}
}

func (Grep) Prepare(ctx context.Context, env *Env, args Args) (*Call, error) {
	expr := args.String("pattern")
	if !args.Bool("case_sensitive") {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression (Go RE2 syntax): %v", err)
	}
	include := strings.ReplaceAll(args.String("include"), `\`, "/")
	exclude := strings.ReplaceAll(args.String("exclude"), `\`, "/")

	p := args.String("path")
	if p == "" {
		p = "."
	}
	abs, inside, err := env.Workspace.Resolve(p)
	if err != nil {
		return nil, err
	}
	shown := env.Workspace.Rel(abs)
	call := &Call{Kind: Read, Summary: fmt.Sprintf("grep %q in %s", args.String("pattern"), shown)}
	if !inside {
		call.Outside, call.SessionKey = []string{abs}, OutsideReadKey
	}
	call.Run = func(ctx context.Context) (Result, error) {
		fold := env.Workspace.fold
		selected := func(rel string) bool {
			name := lastSegment(rel)
			if include != "" && !matchGlob(include, rel, fold) && !matchGlob(include, name, fold) &&
				!matchGlob("**/"+include, rel, fold) {
				return false
			}
			return exclude == "" || !matchGlob(exclude, rel, fold) && !matchGlob(exclude, name, fold) &&
				!matchGlob("**/"+exclude, rel, fold)
		}
		var out strings.Builder
		matches, files := 0, 0
		search := func(fileAbs, rel string) error {
			if matches >= grepMaxMatches {
				return fs.SkipAll
			}
			data, err := os.ReadFile(fileAbs)
			if err != nil || len(data) > maxFileSize || isBinary(data) {
				return nil
			}
			found := false
			sc := bufio.NewScanner(bytes.NewReader(data))
			sc.Buffer(make([]byte, 64*1024), maxFileSize)
			for n := 1; sc.Scan(); n++ {
				line := strings.TrimSuffix(sc.Text(), "\r")
				if !re.MatchString(line) {
					continue
				}
				found = true
				if matches++; matches > grepMaxMatches {
					return fs.SkipAll
				}
				if len(line) > grepMaxLineChars {
					line = firstRunes(line, grepMaxLineChars) + " …"
				}
				fmt.Fprintf(&out, "%s:%d:%s\n", rel, n, line)
			}
			if found {
				files++
			}
			return nil
		}
		fi, err := os.Stat(abs)
		switch {
		case err != nil:
			return Result{}, fmt.Errorf("%s: %w", shown, err)
		case !fi.IsDir():
			err = search(abs, shown)
		default:
			err = searchWalker(env, abs, inside).walk(ctx, abs, func(fileAbs, rel string, d fs.DirEntry) error {
				if d.IsDir() || !selected(rel) {
					return nil
				}
				if !inside {
					return search(fileAbs, filepath.ToSlash(fileAbs)) // a path read_file takes
				}
				return search(fileAbs, rel)
			})
		}
		if err != nil && err != fs.SkipAll && err != errWalkLimit {
			return Result{}, err
		}
		if matches == 0 {
			return Result{Output: fmt.Sprintf("No matches for %q in %s.", args.String("pattern"), shown), Display: "no matches"}, nil
		}
		if matches > grepMaxMatches {
			fmt.Fprintf(&out, "[... more matches not shown; narrow the search with path, include or a more specific pattern]\n")
			matches = grepMaxMatches
		}
		return Result{Output: out.String(), Display: Count(matches, "match", "matches") + " in " + Count(files, "file", "files")}, nil
	}
	return call, nil
}
