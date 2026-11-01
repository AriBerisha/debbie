// Package extension holds the Chrome extension, which "debbie browser
// install" writes out for Chrome to load unpacked (SPEC §20.7). The
// extension itself is plain JavaScript, HTML and CSS; this file only
// embeds it.
package extension

import "embed"

// Files are the extension's files. Its test page, tests/run.html, isn't
// embedded: development loads this folder unpacked.
//
//go:embed manifest.json background.js agent.js rules.js shared.js look.css options.html options.js options.css popup.html popup.js popup.css allow.html allow.js allow.css icons
var Files embed.FS
