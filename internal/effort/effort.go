// Package effort is the reasoning-effort ladder: how hard a model should
// think before it answers. The user picks a level; each adapter fits it to
// what the model supports. The ladder, its ranks and the fitting rule
// follow Qwen Code (packages/core/src/core/reasoning-effort.ts).
package effort

import (
	"slices"
	"strings"
)

// Level is a reasoning-effort level. The zero value, Default, leaves the
// choice to the server.
type Level string

const (
	Default       Level = ""
	None          Level = "none" // no reasoning at all
	Minimal       Level = "minimal"
	Low           Level = "low"
	Medium        Level = "medium"
	High          Level = "high"
	XHigh         Level = "xhigh"
	Max           Level = "max"
	UltraWorkflow Level = "ultraworkflow" // push the model to split work among agents; it reasons at High
)

// Ladder lists the levels at which the model reasons, weakest first.
var Ladder = []Level{Minimal, Low, Medium, High, XHigh, Max}

// rank orders the ladder. The gaps leave room for levels in between.
var rank = map[Level]int{Minimal: 10, Low: 20, Medium: 30, High: 40, XHigh: 60, Max: 70}

// Parse reads a level the way a user writes it. Case, spaces, hyphens and
// underscores don't matter, and a few aliases work: "off" for none, "med",
// "x-high", "extra high", "maximum". "default" or nothing means Default.
func Parse(s string) (Level, bool) {
	key := strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(s)))
	switch key {
	case "", "default":
		return Default, true
	case "none", "off":
		return None, true
	case "minimal", "min":
		return Minimal, true
	case "low":
		return Low, true
	case "medium", "med":
		return Medium, true
	case "high":
		return High, true
	case "xhigh", "extrahigh":
		return XHigh, true
	case "max", "maximum":
		return Max, true
	case "ultraworkflow":
		return UltraWorkflow, true
	}
	return "", false
}

// Real is the level a model is asked to reason at: UltraWorkflow is the
// CLI's own, a push to split work among agents, and reasons at High.
func Real(l Level) Level {
	if l == UltraWorkflow {
		return High
	}
	return l
}

// AtMost returns l, or limit if l is stronger. Default and None stay.
func AtMost(l, limit Level) Level {
	if l = Real(l); rank[l] > rank[limit] {
		return limit
	}
	return l
}

// Fit returns the level to send to a model whose chat template supports the
// given levels, weakest first. A supported level is kept. Otherwise the next
// stronger supported level is used, and a request above the strongest gets
// the strongest. Default and None pass through unchanged, and so does every
// level when nothing is known about the model. UltraWorkflow counts as the
// level it reasons at (see Real), except that when nothing is known it leaves
// the choice to the server: the user chose no level, and a chat template may
// refuse one it doesn't have.
func Fit(l Level, supported []Level) Level {
	push := l == UltraWorkflow
	l = Real(l)
	if l == Default || l == None || slices.Contains(supported, l) {
		return l
	}
	want := rank[l]
	var next, strongest Level
	for _, s := range supported {
		r, known := rank[s]
		if !known {
			continue
		}
		if r >= want && (next == "" || r < rank[next]) {
			next = s
		}
		if strongest == "" || r > rank[strongest] {
			strongest = s
		}
	}
	switch {
	case next != "":
		return next
	case strongest != "":
		return strongest
	case push:
		return Default
	}
	return l
}
