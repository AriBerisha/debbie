package ui

import (
	"fmt"
	"strings"

	"debbie/internal/config"
)

// webModes describes the web access modes (SPEC §19.1).
var webModes = map[string]string{
	config.WebOff:  "no web tools",
	config.WebSafe: "the browser tools ask before opening a site",
	config.WebYolo: "the browser tools run without asking",
}

// webCommand runs /web for both UIs: it reports the mode, or switches to
// off or safe; yolo, which the caller confirms first, is reported with
// yolo set. A message with ok false is a warning.
func webCommand(opts *ChatOptions, arg string) (msg string, ok, yolo bool) {
	w := opts.Web
	if w == nil {
		return "Web access needs the tools, which this chat doesn't have.", false, false
	}
	switch arg = strings.ToLower(arg); arg {
	case "":
		return "Web access: " + w.Mode() + " (" + webModes[w.Mode()] + "). /web off, /web safe or /web yolo switches; /web browser shows Chrome's connection.", true, false
	case config.WebOff, config.WebSafe:
		return setWeb(opts, arg)
	case config.WebYolo:
		return "", true, true
	}
	return fmt.Sprintf("Unknown web mode %q; choose off, safe or yolo.", arg), false, false
}

// setWeb switches web access and says how it stands.
func setWeb(opts *ChatOptions, mode string) (msg string, ok, yolo bool) {
	note := opts.Web.SetMode(mode)
	msg = "Web access: " + mode + " (" + webModes[mode] + ")."
	if note != "" {
		return msg + " " + note + ".", false, false
	}
	if mode != config.WebOff {
		msg += " /web browser shows whether Chrome is connected; /web browser install sets it up."
	}
	return msg, true, false
}

// webYoloWarning is what the user confirms before web yolo.
const webYoloWarning = "Web yolo opens pages on the extension's sites without asking you. Tokens in an address still ask."

// browserCommand runs /web browser: how web access and Chrome's connection
// stand.
func browserCommand(opts *ChatOptions) []string {
	if opts.Web == nil {
		return []string{"The browser needs the tools, which this chat doesn't have."}
	}
	return opts.Web.Describe()
}
