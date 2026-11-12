package setup

import (
	"fmt"
	"slices"
	"strings"

	"debbie/internal/agent"
	"debbie/internal/config"
)

// askAutoCompact asks at what context size a conversation with the
// provider is summarized to make room (SPEC §11), once the model's
// context window is known: the usual size, shares of the window, never,
// or a number typed in. /compact auto changes it later.
func (w *Wizard) askAutoCompact(prov *config.Provider, old *config.Provider) error {
	window := prov.ContextWindow
	type choice struct {
		tokens int
		detail string
	}
	var choices []choice
	add := func(tokens int, detail string) {
		if tokens <= 0 || slices.ContainsFunc(choices, func(c choice) bool { return c.tokens == tokens }) {
			return
		}
		choices = append(choices, choice{tokens, detail})
	}
	usual := agent.AutoCompactAt(agent.DefaultAutoCompact, window)
	def := usual
	if old != nil && old.AutoCompactTokens != nil {
		def = agent.AutoCompactAt(*old.AutoCompactTokens, window)
		add(def, "as it is now")
	}
	add(usual, "the usual")
	if window > 0 {
		add(window*85/100, "85% of the context: the latest it can be, with room for the summary")
		add(window*70/100, "70% of the context")
		add(window*50/100, "50% of the context")
	}
	slices.SortFunc(choices, func(a, b choice) int { return b.tokens - a.tokens })

	c := Choice{Question: "When should the conversation be compacted (summarized to free context) by itself?",
		Other: "At another number of tokens"}
	for i, ch := range choices {
		c.Options = append(c.Options, Option{Label: "At " + kTokens(ch.tokens) + " tokens", Detail: ch.detail, Key: fmt.Sprint(ch.tokens)})
		if ch.tokens == def {
			c.Default = i
		}
	}
	c.Options = append(c.Options, Option{Label: "Never", Detail: "/compact compacts it when you ask", Key: "never"})
	if def == 0 {
		c.Default = len(c.Options) - 1
	}
	for {
		i, typed, err := pick(w.P, c)
		if err != nil {
			return err
		}
		var tokens int
		switch {
		case i < 0:
			n, ok := ParseTokens(typed)
			if !ok {
				w.P.Say("  ! give a number of tokens such as 150k or 1.5m, or 0 for never")
				continue
			}
			tokens = n
		case i < len(choices):
			tokens = choices[i].tokens
		}
		prov.AutoCompactTokens = &tokens
		return nil
	}
}

// ParseTokens reads a token count as the user writes it: 150000, 150k,
// 1.5m, on for the usual, off for none.
func ParseTokens(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "on", "default":
		return agent.DefaultAutoCompact, true
	case "off", "0", "none", "never":
		return 0, true
	}
	n, ok := parseCount(s)
	return n, ok && n >= config.MinAutoCompactTokens
}
