//go:build !windows

package tools

import (
	"errors"
	"path/filepath"
	"strings"
)

func resolveExisting(p string) (string, error) {
	return filepath.EvalSymlinks(p)
}

// checkPathSyntax rejects paths the file tools never accept.
func checkPathSyntax(p string) error {
	if strings.ContainsRune(p, 0) {
		return errors.New("the path contains a NUL character")
	}
	return nil
}
