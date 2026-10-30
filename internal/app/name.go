// Package app holds the program's name and version.
package app

import "strings"

// Name is the program's name: the binary, the config and state directories,
// and the prefix of its environment variables. It is set here and nowhere
// else so the program can be renamed in one place.
const Name = "debbie"

// FormerNames are the names the program had before, whose configuration and
// files a newer version takes over: it was debbie-cli until 2026-10-30.
var FormerNames = []string{"debbie-cli"}

// EnvVar returns the name of one of the program's own environment variables.
// EnvVar("CONFIG") is "DEBBIE_CONFIG".
func EnvVar(suffix string) string { return envVar(Name, suffix) }

// FormerEnvVars returns the variable's names under the former names, which
// are still read: FormerEnvVars("CONFIG") is ["DEBBIE_CLI_CONFIG"].
func FormerEnvVars(suffix string) []string {
	var names []string
	for _, n := range FormerNames {
		names = append(names, envVar(n, suffix))
	}
	return names
}

func envVar(name, suffix string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_" + suffix
}
