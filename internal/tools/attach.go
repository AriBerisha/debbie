package tools

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// maxCompleteVisits bounds the entries one completion looks at, so a huge
// workspace can't stall typing.
const maxCompleteVisits = 20000

// junkDirs and junkExts are left out of completion even where no
// .gitignore says so: caches, dependencies and compiled files, which
// can't be attached anyway.
var (
	junkDirs = map[string]bool{"__pycache__": true, "node_modules": true, ".git": true, ".hg": true, ".svn": true,
		".venv": true, ".mypy_cache": true, ".pytest_cache": true, ".tox": true, ".idea": true, ".vs": true}
	junkExts = map[string]bool{".pyc": true, ".pyo": true, ".class": true, ".o": true, ".obj": true, ".exe": true, ".dll": true, ".so": true, ".dylib": true}
)

// junk reports whether completion leaves an entry out.
func junk(d fs.DirEntry) bool {
	if d.IsDir() {
		return junkDirs[d.Name()]
	}
	return junkExts[strings.ToLower(filepath.Ext(d.Name()))]
}

// Complete suggests workspace paths for an @path mention (SPEC §12), with
// '/' separators, directories ending in '/', and ignored files left out.
// A query with a '/' lists that directory's entries starting with what
// follows the last '/'; one without looks through the whole workspace for
// names that start with it, then names that contain it, shorter paths
// first.
func (w *Workspace) Complete(query string, max int) []string {
	if strings.Contains(query, "..") || filepath.IsAbs(query) || strings.ContainsAny(query, `\:`) {
		return nil
	}
	fold := func(s string) string {
		if w.fold {
			return strings.ToLower(s)
		}
		return s
	}
	dir, base := path.Split(query)
	if dir != "" {
		abs, inside, err := w.Resolve(filepath.FromSlash(dir))
		if err != nil || !inside {
			return nil
		}
		var out []string
		w.walker().walk(context.Background(), abs, func(_, rel string, d fs.DirEntry) error {
			if len(out) >= max {
				return fs.SkipAll
			}
			if junk(d) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(fold(d.Name()), fold(base)) {
				if d.IsDir() {
					rel += "/"
				}
				out = append(out, rel)
			}
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		})
		return out
	}

	type hit struct {
		rel   string
		start bool // the name starts with the query
	}
	var hits []hit
	visits := 0
	q := fold(base)
	w.walker().walk(context.Background(), w.root, func(_, rel string, d fs.DirEntry) error {
		if visits++; visits > maxCompleteVisits {
			return fs.SkipAll
		}
		if junk(d) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := fold(d.Name())
		if i := strings.Index(name, q); i >= 0 {
			if d.IsDir() {
				rel += "/"
			}
			hits = append(hits, hit{rel, i == 0})
		}
		return nil
	})
	slices.SortStableFunc(hits, func(a, b hit) int {
		switch {
		case a.start != b.start:
			if a.start {
				return -1
			}
			return 1
		case strings.Count(a.rel, "/") != strings.Count(b.rel, "/"):
			return strings.Count(a.rel, "/") - strings.Count(b.rel, "/")
		}
		return len(a.rel) - len(b.rel)
	})
	var out []string
	for _, h := range hits[:min(len(hits), max)] {
		out = append(out, h.rel)
	}
	return out
}

// Attachment is a file attached to a message with @path.
type Attachment struct {
	Path  string // relative to the workspace, with '/' separators
	Lines int
	Text  string // read_file's limits applied
}

// Attach reads a file named in an @path mention, as read_file would, with
// the same limits. Only files inside the workspace can be attached.
func (w *Workspace) Attach(p string) (Attachment, error) {
	abs, inside, err := w.Resolve(filepath.FromSlash(p))
	if err != nil {
		return Attachment{}, err
	}
	if !inside {
		return Attachment{}, fmt.Errorf("%s is outside the workspace", p)
	}
	shown := w.Rel(abs)
	if fileImageType(abs) != "" {
		return Attachment{Path: shown, Text: "[This is an image, so its content isn't here. Call read_file on it to see it, if you can see images.]"}, nil
	}
	f, _, err := readText(abs, shown)
	if err != nil {
		return Attachment{}, err
	}
	lines := strings.Split(strings.TrimSuffix(f.text, "\n"), "\n")
	total := len(lines)
	if len(lines) > readDefaultLines {
		lines = lines[:readDefaultLines]
	}
	for i, line := range lines {
		if len(line) > readMaxLineChars {
			lines[i] = firstRunes(line, readMaxLineChars) + " … [line cut]"
		}
	}
	text := strings.Join(lines, "\n")
	if total > len(lines) {
		text += fmt.Sprintf("\n[… %d more lines; read_file can read the rest]", total-len(lines))
	}
	return Attachment{Path: shown, Lines: total, Text: text}, nil
}
