package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"debbie/internal/app"
	"debbie/internal/browser"
)

// hostCommand runs the native messaging host that Chrome starts (SPEC
// §20.2). Its stdout carries messages to the extension only.
func hostCommand(origin string, stdin io.Reader, stdout, stderr io.Writer) int {
	dir, err := browser.RunDir()
	if err != nil {
		fmt.Fprintf(stderr, "%s host: %v\n", app.Name, err)
		return exitError
	}
	if err := browser.RunHost(origin, stdin, stdout, dir); err != nil {
		fmt.Fprintf(stderr, "%s host: %v\n", app.Name, err)
		return exitError
	}
	return exitOK
}

// browserUsage describes the browser subcommand.
func browserUsage() string {
	n := app.Name
	return "usage: " + n + " browser install [--dev]   write the Chrome extension and register its host\n" +
		"       " + n + " browser status            check each part of the install\n" +
		"       " + n + " browser uninstall         remove the extension's files and the host\n\n" +
		"The extension lets a session with web access on (/web safe) open and read pages\n" +
		"on the sites you allow, youtube.com to begin with, in a tab of its own.\n"
}

// browserCommand runs "browser install|uninstall|status" (SPEC §20.7).
func browserCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, browserUsage())
		return exitConfig
	}
	in, err := browser.NewInstaller()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
		return exitError
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, browserUsage())
		return exitOK
	case "install":
		flags := flag.NewFlagSet(app.Name+" browser install", flag.ContinueOnError)
		flags.SetOutput(stderr)
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() > 0 {
			return exitConfig
		}
		got, err := in.Install()
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
			return exitError
		}
		loaded := "If it is loaded already, click its reload button instead."
		if got.Former != "" {
			loaded = "Chrome replaces the copy loaded from\n       " + got.Former + "\n     with it: the same extension, renamed."
		}
		fmt.Fprintf(stdout, "Installed the %s browser extension.\n"+
			"  extension   %s\n"+
			"  host        %s\n"+
			"  registered  %s\n\n"+
			"To load it in Chrome:\n"+
			"  1. Open chrome://extensions\n"+
			"  2. Turn on Developer mode (top right)\n"+
			"  3. Click \"Load unpacked\" and choose the folder\n"+
			"       %s\n"+
			"     %s\n"+
			"  4. Check that its ID is %s\n\n"+
			"It opens https pages on: %s. Its options page (Details, then Extension\n"+
			"options) adds and removes sites. In a session, /web safe turns web access on;\n"+
			"/web browser then shows whether Chrome is connected. In a session, /web browser install does\n"+
			"all this step by step and says when Chrome connects.\n",
			app.Name, got.Extension, in.HostExe(), in.Where, got.Extension, loaded, got.ID, strings.Join(got.Sites, ", "))
		return exitOK
	case "uninstall":
		if len(args) > 1 {
			return exitConfig
		}
		if err := in.Uninstall(); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
			return exitError
		}
		fmt.Fprintf(stdout, "Removed the extension's files and the host, and unregistered it.\n"+
			"Remove the extension from chrome://extensions too. The key stays in %s,\n"+
			"so installing again keeps the extension's ID.\n", in.KeyFile)
		return exitOK
	case "status":
		ok := true
		for _, c := range in.Status() {
			mark := "ok     "
			if !c.OK {
				mark, ok = "missing", false
			}
			fmt.Fprintf(stdout, "%s  %-14s %s\n", mark, c.Name, c.Detail)
		}
		if dir, err := browser.RunDir(); err == nil {
			if o, held := browser.HolderOf(dir); held {
				fmt.Fprintf(stdout, "\nThe session in %s (process %d) has web access on.\n", o.Workspace, o.PID)
			} else {
				fmt.Fprintln(stdout, "\nNo session has web access on: /web safe in a session turns it on.")
			}
		}
		if !ok {
			return exitError
		}
		return exitOK
	}
	fmt.Fprintf(stderr, "%s browser: unknown command %q\n\n%s", app.Name, args[0], browserUsage())
	return exitConfig
}
