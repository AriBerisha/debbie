package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Workspace resolves the paths that tools receive against the directory the
// agent works in, its root (SPEC §3.5).
type Workspace struct {
	root string // absolute, with symbolic links and junctions resolved
	fold bool   // the file system ignores letter case
}

// NewWorkspace returns a workspace rooted at dir.
func NewWorkspace(dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root, err := realPath(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace %s: %w", abs, err)
	}
	return &Workspace{root: root, fold: runtime.GOOS == "windows" || runtime.GOOS == "darwin"}, nil
}

// Root returns the workspace directory.
func (w *Workspace) Root() string { return w.root }

// Resolve turns a path from the model into an absolute path, relative paths
// being relative to the root. Symbolic links and junctions in the existing
// part of the path are followed, so the result is where a read or write
// would really land; inside reports whether that is within the root.
func (w *Workspace) Resolve(p string) (abs string, inside bool, err error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false, errors.New("the path is empty")
	}
	if err := checkPathSyntax(p); err != nil {
		return "", false, err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.root, p)
	}
	abs, err = realPath(filepath.Clean(p))
	if err != nil {
		return "", false, err
	}
	return abs, w.contains(abs), nil
}

// ResolveForWrite is Resolve for tools that write. Writes never follow a
// symbolic link or junction out of the workspace (SPEC §3.5): a path that
// is inside as written but leads outside is refused. A path outside as
// written is fine; it needs approval like any other.
func (w *Workspace) ResolveForWrite(p string) (abs string, inside bool, err error) {
	abs, inside, err = w.Resolve(p)
	if err != nil || inside {
		return abs, inside, err
	}
	written := strings.TrimSpace(p)
	if !filepath.IsAbs(written) {
		written = filepath.Join(w.root, written)
	}
	if w.contains(filepath.Clean(written)) {
		return "", false, fmt.Errorf("%s leads out of the workspace through a link, to %s; writes don't follow links out of the workspace", p, abs)
	}
	return abs, false, nil
}

// contains reports whether abs, a resolved absolute path, is the root or
// lies below it.
func (w *Workspace) contains(abs string) bool {
	rel, err := filepath.Rel(w.root, abs) // case-insensitive on Windows
	if err != nil {
		return false
	}
	return rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Rel shows a path relative to the root with '/' separators, or as it is
// when it lies outside.
func (w *Workspace) Rel(abs string) string {
	if !w.contains(abs) {
		return abs
	}
	rel, _ := filepath.Rel(w.root, abs)
	return filepath.ToSlash(rel)
}

// walker returns a walker over the workspace.
func (w *Workspace) walker() *walker { return &walker{root: w.root, fold: w.fold} }

// realPath resolves the symbolic links and junctions in the longest
// existing prefix of an absolute, clean path and appends the rest.
func realPath(p string) (string, error) {
	var rest []string
	for cur := p; ; {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := resolveExisting(cur)
			if err != nil {
				return "", fmt.Errorf("can't resolve %s: %w", cur, err)
			}
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil // nothing exists, not even the volume
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}
