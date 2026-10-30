//go:build !windows

package config

import (
	"fmt"
	"os"
	"strings"
)

// The umask can only remove permission bits, so these modes are never
// loosened.

func mkdirPrivate(dir string) error {
	return os.Mkdir(dir, 0o700)
}

func createPrivateFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
}

func replaceFile(from, to string) error {
	return os.Rename(from, to)
}

// syncDir flushes a directory, making a rename inside it durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// checkPrivate returns a warning when users other than the owner may read or
// change the file.
func checkPrivate(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if perm := fi.Mode().Perm(); perm&0o066 != 0 {
		quoted := "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
		return fmt.Sprintf("%s can be read or changed by other users (mode %04o); restrict it with: chmod 600 %s", path, perm, quoted), nil
	}
	return "", nil
}
