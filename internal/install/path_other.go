//go:build !windows

package install

import "errors"

// Outside Windows the PATH lives in the shells' start-up files, which
// addToPath writes; there is no user PATH to read or set.

var errNoUserPath = errors.New("only Windows keeps a user PATH")

func readUserPath() (string, bool, error)      { return "", false, errNoUserPath }
func writeUserPath(value string, _ bool) error { return errNoUserPath }
