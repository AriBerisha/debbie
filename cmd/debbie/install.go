package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"debbie/internal/app"
	"debbie/internal/config"
	"debbie/internal/install"
)

// installCommand runs "debbie install": the program into the user's own
// folder for programs, which joins their PATH, so that typing debbie starts
// it in any new terminal (SPEC §16.1). Running it again updates it.
func installCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(app.Name+" install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "install into this folder instead of the usual one")
	noPath := flags.Bool("no-path", false, "leave the PATH as it is")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s install [--dir DIR] [--no-path]\n\n"+
			"Puts %s in your own folder for programs and that folder on your PATH, so that\n"+
			"typing %s starts it: %%LOCALAPPDATA%%\\Programs\\%s on Windows, ~/.local/bin elsewhere.\n"+
			"No administrator rights are needed. %s uninstall takes it away again.\n\n", app.Name, app.Name, app.Name, app.Name, app.Name)
		flags.PrintDefaults()
	}
	in, code := installer(flags, args, dir, stderr)
	if in == nil {
		return code
	}
	done, err := in.Install(!*noPath)
	for _, l := range done.Lines {
		fmt.Fprintln(stdout, l)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
		return exitError
	}
	switch {
	case *noPath:
		fmt.Fprintf(stdout, "\nYour PATH is as it was: start %s as %s, or add %s to your PATH.\n", app.Name, in.Command(), in.Dir)
	case done.NewShell:
		fmt.Fprintf(stdout, "\nOpen a new terminal, then type %s to start.\n", app.Name)
		if in.GOOS == "windows" {
			fmt.Fprintln(stdout, "A program that was open already, VS Code for one, gives its terminals the new PATH once you restart it.")
		}
	default:
		fmt.Fprintf(stdout, "\nType %s to start.\n", app.Name)
	}
	return exitOK
}

// uninstallCommand runs "debbie uninstall": the installed program and its
// place on the PATH go; the settings stay.
func uninstallCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(app.Name+" uninstall", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "the folder it was installed in, if not the usual one")
	in, code := installer(flags, args, dir, stderr)
	if in == nil {
		return code
	}
	done, err := in.Uninstall()
	for _, l := range done.Lines {
		fmt.Fprintln(stdout, l)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
		return exitError
	}
	if d, err := config.Dir(); err == nil {
		fmt.Fprintf(stdout, "Your settings stay in %s; delete that folder to remove them too.\n", d)
	}
	return exitOK
}

// installer parses an install command's flags and returns the installer,
// or nil and the exit code.
func installer(flags *flag.FlagSet, args []string, dir *string, stderr io.Writer) (*install.Installer, int) {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, exitOK
		}
		return nil, exitConfig
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", app.Name, flags.Arg(0))
		return nil, exitConfig
	}
	in, err := install.New()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
		return nil, exitError
	}
	if *dir != "" {
		abs, err := filepath.Abs(*dir)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
			return nil, exitConfig
		}
		in.Dir = abs
	}
	return in, 0
}
