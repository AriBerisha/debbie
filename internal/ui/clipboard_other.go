//go:build !windows

package ui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// copyText puts text on the clipboard with the system's clipboard program,
// run with env, the environment commands get (without API keys); without
// one, it asks the terminal to, with OSC 52, written to out. It returns
// where the text went.
func copyText(text string, env []string, out io.Writer) (string, error) {
	var programs [][]string
	switch {
	case runtime.GOOS == "darwin":
		programs = [][]string{{"pbcopy"}}
	case os.Getenv("WAYLAND_DISPLAY") != "":
		programs = [][]string{{"wl-copy"}}
	case os.Getenv("DISPLAY") != "":
		programs = [][]string{{"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}}
	}
	if env != nil {
		for _, p := range programs {
			path, err := exec.LookPath(p[0])
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			cmd := exec.CommandContext(ctx, path, p[1:]...)
			cmd.Env, cmd.Stdin = env, strings.NewReader(text)
			err = cmd.Run()
			cancel()
			if err == nil {
				return p[0], nil
			}
		}
	}
	fmt.Fprintf(out, "\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(text)))
	return "the terminal's clipboard (OSC 52)", nil
}

// pasteText reads the clipboard's text with the system's clipboard
// program, run with env, the environment commands get (without API keys).
func pasteText(env []string) (string, error) {
	var programs [][]string
	switch {
	case runtime.GOOS == "darwin":
		programs = [][]string{{"pbpaste"}}
	case os.Getenv("WAYLAND_DISPLAY") != "":
		programs = [][]string{{"wl-paste", "--no-newline"}}
	case os.Getenv("DISPLAY") != "":
		programs = [][]string{{"xclip", "-selection", "clipboard", "-o"}, {"xsel", "--clipboard", "--output"}}
	}
	if env != nil {
		for _, p := range programs {
			path, err := exec.LookPath(p[0])
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			cmd := exec.CommandContext(ctx, path, p[1:]...)
			cmd.Env = env
			out, err := cmd.Output()
			cancel()
			if err == nil {
				return string(out), nil
			}
		}
	}
	return "", errors.New("no clipboard program here: paste with your terminal's paste keys")
}
