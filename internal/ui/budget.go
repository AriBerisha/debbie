package ui

import (
	"fmt"

	"debbie/internal/config"
	"debbie/internal/setup"
)

// /effort budget: a cap on how long the model thinks per reply, for llama-server
// (SPEC §4.3).

// budgetUsage says what /effort budget takes.
const budgetUsage = "Give a number of tokens such as 8k or 16000, or auto for no cap."

// reasoningBudget runs /effort budget for either UI: with no argument it says
// whether the model's thinking per reply is capped; with a number of
// tokens (16000 or 16k) or auto, it changes that for the session and
// saves it for the provider. It returns what to tell the user, and false
// for a bad argument.
func reasoningBudget(opts *ChatOptions, arg string) (string, bool) {
	if arg == "" {
		if n := opts.Session.ReasoningBudget(); n > 0 {
			return fmt.Sprintf("Thinking is capped at %s tokens per reply.", kTokens(n)), true
		}
		return "Thinking per reply isn't capped: the server decides (auto).", true
	}
	n, ok := setup.ParseBudget(arg)
	if !ok {
		return budgetUsage, false
	}
	if opts.Providers != nil && n > 0 {
		if p := opts.Providers.Config.Providers[opts.Status.Provider]; p != nil && p.Adapter == config.AdapterAnthropic {
			return "Claude's thinking follows the effort level (/effort); a budget applies to llama-server.", false
		}
	}
	opts.Session.SetReasoningBudget(n)
	saved := ""
	if opts.Providers != nil {
		if err := opts.Providers.SetReasoningBudget(opts.Status.Provider, n); err != nil {
			saved = " (for this session; not saved: " + err.Error() + ")"
		} else {
			saved = ", saved for " + opts.Status.Provider
		}
	}
	if n == 0 {
		return "Thinking per reply isn't capped" + saved + ": the server decides.", true
	}
	return fmt.Sprintf("Thinking is capped at %s tokens per reply%s.", kTokens(n), saved), true
}
