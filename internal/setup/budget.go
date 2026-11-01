package setup

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"debbie/internal/config"
)

// budgetSizes are the caps the wizard offers, in tokens.
var budgetSizes = []int{4000, 8000, 16000, 32000}

// askReasoningBudget asks whether to cap how long the model thinks per
// reply (SPEC §4.3), for a server that takes a reasoning budget per
// request, where thinking otherwise runs as long as the model likes: no
// cap, a few sizes under half the context window, or a number typed in.
// /budget changes it later.
func (w *Wizard) askReasoningBudget(prov *config.Provider, old *config.Provider) error {
	window := prov.ContextWindow
	def := 0
	if old != nil {
		def = old.ReasoningBudgetTokens
	}
	sizes := slices.Clone(budgetSizes)
	if def > 0 && !slices.Contains(sizes, def) {
		sizes = append(sizes, def)
		slices.Sort(sizes)
	}
	c := Choice{Question: "Cap how long the model thinks per reply (a reasoning budget)?", Other: "Another number of tokens"}
	c.Options = append(c.Options, Option{Label: "No cap", Detail: "auto: the server decides, and a hard task may think until the context is full", Key: "auto"})
	for _, n := range sizes {
		if window > 0 && n*2 > window && n != def {
			continue
		}
		detail := ""
		if n == def {
			detail = "as it is now"
			c.Default = len(c.Options)
		}
		c.Options = append(c.Options, Option{Label: kTokens(n) + " tokens", Detail: detail, Key: strconv.Itoa(n)})
	}
	for {
		i, typed, err := pick(w.P, c)
		if err != nil {
			return err
		}
		n := 0
		switch {
		case i < 0:
			var ok bool
			if n, ok = ParseBudget(typed); !ok {
				w.P.Say("  ! give a number of tokens such as 8k or 16000, or auto for no cap")
				continue
			}
		case i > 0:
			n, _ = strconv.Atoi(c.Options[i].Key)
		}
		prov.ReasoningBudgetTokens = n
		return nil
	}
}

// ParseBudget reads a reasoning budget as the user writes it: 16000, 16k,
// or auto (also default, off or unlimited) for none.
func ParseBudget(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "auto", "default", "off", "unlimited":
		return 0, true
	}
	n, ok := parseCount(s)
	return n, ok && n >= 1
}

// parseCount reads a count as people write it: 150000, 150k, 1.5m. It
// must be positive.
func parseCount(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1000, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1_000_000, strings.TrimSuffix(s, "m")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return int(f * mult), true
}

// describeBudget says what a reasoning budget means, for the summary.
func describeBudget(n int) string {
	if n <= 0 {
		return "no cap on thinking"
	}
	return fmt.Sprintf("thinking capped at %s tokens per reply", kTokens(n))
}
