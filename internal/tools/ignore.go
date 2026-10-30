package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// ignoreRule is one line of a .gitignore file, following gitignore(5).
type ignoreRule struct {
	pattern  string // without a leading or trailing slash
	negate   bool   // "!pattern" re-includes
	dirOnly  bool   // "pattern/" matches directories only
	anchored bool   // a slash before the end: relative to the file's directory
	base     string // directory of the .gitignore, relative to the walk root, '/'-separated
}

// parseIgnore reads the rules of a .gitignore file in directory base.
func parseIgnore(data []byte, base string) []ignoreRule {
	var rules []ignoreRule
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" || line[0] == '#' {
			continue
		}
		// Trailing spaces don't count unless escaped with a backslash.
		for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
			line = line[:len(line)-1]
		}
		r := ignoreRule{base: base}
		switch {
		case line[0] == '!':
			r.negate, line = true, line[1:]
		case strings.HasPrefix(line, `\!`), strings.HasPrefix(line, `\#`):
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly, line = true, strings.TrimRight(line, "/")
		}
		r.anchored = strings.Contains(line, "/")
		r.pattern = strings.TrimPrefix(line, "/")
		if r.pattern != "" {
			rules = append(rules, r)
		}
	}
	return rules
}

// matches reports whether the rule applies to rel, a '/'-separated path
// relative to the walk root.
func (r ignoreRule) matches(rel string, isDir, fold bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if r.base != "" {
		var ok bool
		if rel, ok = strings.CutPrefix(rel, r.base+"/"); !ok {
			return false
		}
	}
	if r.anchored {
		return matchGlob(r.pattern, rel, fold)
	}
	return matchGlob(r.pattern, path.Base(rel), fold)
}

// ignored applies rules in order; the last one that matches decides.
func ignored(rules []ignoreRule, rel string, isDir, fold bool) bool {
	ign := false
	for _, r := range rules {
		if r.matches(rel, isDir, fold) {
			ign = !r.negate
		}
	}
	return ign
}

// errWalkLimit stops a walk that has visited too many entries.
var errWalkLimit = errors.New("too many files")

// maxWalkEntries bounds how many entries a single walk visits.
const maxWalkEntries = 200_000

// walker visits the files in the workspace, skipping .git directories and
// whatever .gitignore files exclude. Symbolic links and junctions are
// reported but never followed.
type walker struct {
	root string // absolute
	fold bool   // ignore letter case when matching
}

// walkFunc receives each entry: its absolute path, its path relative to the
// root with '/' separators, and the entry itself. Returning fs.SkipDir for
// a directory skips its contents; fs.SkipAll ends the walk.
type walkFunc func(abs, rel string, d fs.DirEntry) error

// walk visits everything under dir, an absolute path inside the root, in
// lexical order. Rules from .gitignore files between the root and dir apply
// as well.
func (w *walker) walk(ctx context.Context, dir string, fn walkFunc) error {
	rel, err := filepath.Rel(w.root, dir)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		rel = ""
	}
	// Collect the rules of the directories above dir.
	var rules []ignoreRule
	parts := []string{}
	if rel != "" {
		parts = strings.Split(rel, "/")
	}
	for i := 0; i < len(parts); i++ {
		base := strings.Join(parts[:i], "/")
		rules = append(rules, w.readRules(base)...)
	}
	visited := 0
	err = w.walkDir(ctx, rel, rules, fn, &visited)
	if errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

func (w *walker) readRules(relDir string) []ignoreRule {
	data, err := os.ReadFile(filepath.Join(w.root, filepath.FromSlash(relDir), ".gitignore"))
	if err != nil {
		return nil
	}
	return parseIgnore(data, relDir)
}

func (w *walker) walkDir(ctx context.Context, relDir string, rules []ignoreRule, fn walkFunc, visited *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rules = append(slices.Clip(rules), w.readRules(relDir)...)
	absDir := filepath.Join(w.root, filepath.FromSlash(relDir))
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil // unreadable directories are skipped
	}
	for _, d := range entries {
		name := d.Name()
		if name == ".git" {
			continue
		}
		rel := name
		if relDir != "" {
			rel = relDir + "/" + name
		}
		isDir := d.IsDir()
		if ignored(rules, rel, isDir, w.fold) {
			continue
		}
		if *visited++; *visited > maxWalkEntries {
			return errWalkLimit
		}
		err := fn(filepath.Join(absDir, name), rel, d)
		if errors.Is(err, fs.SkipDir) {
			continue
		}
		if err != nil {
			return err
		}
		if isDir && d.Type()&fs.ModeSymlink == 0 {
			if err := w.walkDir(ctx, rel, rules, fn, visited); err != nil {
				return err
			}
		}
	}
	return nil
}
