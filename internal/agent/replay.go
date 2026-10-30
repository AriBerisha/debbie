package agent

import (
	"strings"

	"debbie/internal/prompt"
	"debbie/internal/provider"
)

// Step is a part of a saved conversation as the UI shows it again when the
// conversation is resumed (SPEC §12).
type Step struct {
	User       string    // what the user said, without the files attached
	Attached   int       // how many files were attached to it
	Text       string    // what the model said
	Calls      []ToolRun // the model's tool calls, as they ended
	Summarized bool      // what came before was compacted into a summary
}

// Replay turns a conversation back into the steps the UI showed: the
// user's messages, the model's replies and its calls, each with how it
// ended. Reasoning is left out.
func Replay(history []provider.Message) []Step {
	results := map[string]*provider.ToolResult{}
	for _, m := range history {
		for _, p := range m.Parts {
			if r := p.ToolResult; r != nil {
				results[r.CallID] = r
			}
		}
	}
	var steps []Step
	for _, m := range history {
		text := m.Text()
		switch m.Role {
		case provider.RoleUser:
			switch {
			case strings.TrimSpace(text) == "": // only tool results
			case prompt.IsCompactedSummary(text):
				steps = append(steps, Step{Summarized: true})
			default:
				user, attached := text, 0
				if i := strings.Index(user, "<attached_file"); i >= 0 {
					attached = strings.Count(user[i:], "<attached_file")
					user = user[:i]
				}
				steps = append(steps, Step{User: strings.TrimSpace(user), Attached: attached})
			}
		case provider.RoleAssistant:
			if text == prompt.CompactionAck() {
				continue
			}
			st := Step{Text: strings.TrimSpace(text)}
			for _, p := range m.Parts {
				c := p.ToolCall
				if c == nil {
					continue
				}
				run := ToolRun{Name: c.Name, Summary: fallbackSummary(*c), Status: RunCancelled}
				if r := results[c.ID]; r != nil {
					run.Status, run.Output = RunOK, r.Content
					if r.IsError {
						run.Status, run.Error, run.Output = RunFailed, r.Content, ""
					}
				}
				st.Calls = append(st.Calls, run)
			}
			if st.Text != "" || len(st.Calls) > 0 {
				steps = append(steps, st)
			}
		}
	}
	return steps
}
