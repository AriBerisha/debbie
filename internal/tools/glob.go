package tools

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"debbie/internal/prompt"
)

const globMaxResults = 200

// Glob finds files by pattern.
type Glob struct{}

func (Glob) Name() string        { return "glob" }
func (Glob) Kind() Kind          { return Read }
func (Glob) Description() string { return prompt.Tool("glob") }

func (Glob) Params() []Param {
	return []Param{
		{Name: "pattern", Type: "string", Required: true, Description: "A glob such as **/*.go."},
		{Name: "path", Type: "string", Description: "The directory to search; the workspace root when omitted.",
			Aliases: []string{"dir_path", "directory"}},
	}
}

func (Glob) Prepare(ctx context.Context, env *Env, args Args) (*Call, error) {
	pattern := strings.TrimPrefix(strings.ReplaceAll(args.String("pattern"), `\`, "/"), "./")
	if pattern == "" {
		return nil, fmt.Errorf("the pattern is empty")
	}
	dir, base, inside, err := searchDir(env, args.String("path"))
	if err != nil {
		return nil, err
	}
	call := &Call{Kind: Read, Summary: fmt.Sprintf("glob %s in %s", pattern, base.shown)}
	if !inside {
		call.Outside, call.SessionKey = []string{dir}, OutsideReadKey
	}
	call.Run = func(ctx context.Context) (Result, error) {
		type hit struct {
			rel string
			mod time.Time
		}
		var hits []hit
		err := searchWalker(env, dir, inside).walk(ctx, dir, func(fileAbs, rel string, d fs.DirEntry) error {
			if d.IsDir() {
				return nil
			}
			below := strings.TrimPrefix(rel, base.prefix)
			if matchGlob(pattern, below, env.Workspace.fold) || !strings.Contains(pattern, "/") && matchGlob(pattern, lastSegment(below), env.Workspace.fold) {
				var mod time.Time
				if info, err := d.Info(); err == nil {
					mod = info.ModTime()
				}
				if !inside {
					rel = filepath.ToSlash(fileAbs) // a path read_file takes
				}
				hits = append(hits, hit{rel, mod})
			}
			return nil
		})
		if err != nil && err != errWalkLimit {
			return Result{}, err
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].mod.After(hits[j].mod) })
		if len(hits) == 0 {
			return Result{Output: fmt.Sprintf("No files match %s in %s.", pattern, base.shown), Display: "no files"}, nil
		}
		var b strings.Builder
		for i, h := range hits {
			if i == globMaxResults {
				fmt.Fprintf(&b, "[... %d more files; narrow the pattern]\n", len(hits)-globMaxResults)
				break
			}
			b.WriteString(h.rel + "\n")
		}
		return Result{Output: b.String(), Display: Count(len(hits), "file", "files")}, nil
	}
	return call, nil
}

func lastSegment(p string) string { return p[strings.LastIndexByte(p, '/')+1:] }

// searchBase describes the directory a search starts in.
type searchBase struct {
	shown  string // for messages
	prefix string // its path relative to the root, with a trailing slash, or ""
}

// searchDir resolves the directory a search tool works in, which may lie
// outside the workspace: a search there asks like any read outside it.
func searchDir(env *Env, p string) (string, searchBase, bool, error) {
	if p == "" {
		p = "."
	}
	abs, inside, err := env.Workspace.Resolve(p)
	if err != nil {
		return "", searchBase{}, false, err
	}
	rel := env.Workspace.Rel(abs)
	base := searchBase{shown: rel}
	if inside && rel != "." {
		base.prefix = rel + "/"
	}
	return abs, base, inside, nil
}

// searchWalker walks a search: the workspace's walker inside it, and outside
// it one rooted at the folder searched, whose own .gitignore files apply.
func searchWalker(env *Env, dir string, inside bool) *walker {
	if inside {
		return env.Workspace.walker()
	}
	return &walker{root: dir, fold: env.Workspace.fold}
}
