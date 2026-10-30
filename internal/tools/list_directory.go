package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"debbie/internal/prompt"
)

const listMaxEntries = 500

// ListDirectory lists a directory's entries.
type ListDirectory struct{}

func (ListDirectory) Name() string        { return "list_directory" }
func (ListDirectory) Kind() Kind          { return Read }
func (ListDirectory) Description() string { return prompt.Tool("list_directory") }

func (ListDirectory) Params() []Param {
	return []Param{{Name: "path", Type: "string", Description: "The directory; the workspace root when omitted.",
		Aliases: []string{"dir_path", "directory", "dir"}}}
}

func (ListDirectory) Prepare(ctx context.Context, env *Env, args Args) (*Call, error) {
	p := args.String("path")
	if p == "" {
		p = "."
	}
	abs, inside, err := env.Workspace.Resolve(p)
	if err != nil {
		return nil, err
	}
	shown := env.Workspace.Rel(abs)
	call := &Call{Kind: Read, Summary: "list_directory " + shown}
	if !inside {
		call.Outside, call.SessionKey = []string{abs}, OutsideReadKey
	}
	call.Run = func(ctx context.Context) (Result, error) {
		fi, err := os.Stat(abs)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", shown, err)
		}
		if !fi.IsDir() {
			return Result{}, fmt.Errorf("%s is a file, not a directory; use read_file", shown)
		}
		type entry struct {
			name string
			dir  bool
			info string
		}
		var entries []entry
		hidden := 0
		visit := func(entryAbs string, d fs.DirEntry) error {
			e := entry{name: d.Name(), dir: d.IsDir()}
			switch info, err := d.Info(); {
			case d.Type()&fs.ModeSymlink != 0:
				target, _ := os.Readlink(entryAbs)
				e.info = "link to " + target
			case e.dir:
				e.info = "directory"
			case err == nil:
				e.info = sizeText(info.Size())
			}
			entries = append(entries, e)
			return nil
		}
		if inside {
			err = env.Workspace.walker().walk(ctx, abs, func(entryAbs, _ string, d fs.DirEntry) error {
				if err := visit(entryAbs, d); err != nil {
					return err
				}
				if d.IsDir() {
					return fs.SkipDir // one level only
				}
				return nil
			})
			all, _ := os.ReadDir(abs)
			hidden = len(all) - len(entries)
		} else {
			// Outside the workspace there are no .gitignore rules to apply.
			var all []fs.DirEntry
			all, err = os.ReadDir(abs)
			for _, d := range all {
				visit(filepath.Join(abs, d.Name()), d)
			}
		}
		if err != nil {
			return Result{}, err
		}
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].dir != entries[j].dir {
				return entries[i].dir
			}
			return strings.ToLower(entries[i].name) < strings.ToLower(entries[j].name)
		})
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %d entries\n", shown, len(entries))
		for i, e := range entries {
			if i == listMaxEntries {
				fmt.Fprintf(&b, "[... %d more entries not shown]\n", len(entries)-listMaxEntries)
				break
			}
			name := e.name
			if e.dir {
				name += "/"
			}
			fmt.Fprintf(&b, "%s\t%s\n", name, e.info)
		}
		if hidden > 0 {
			fmt.Fprintf(&b, "(%d entries hidden by .gitignore rules or .git)\n", hidden)
		}
		return Result{Output: b.String(), Display: Count(len(entries), "entry", "entries")}, nil
	}
	return call, nil
}
