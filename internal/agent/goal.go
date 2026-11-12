package agent

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"

	"debbie/internal/prompt"
	"debbie/internal/provider"
)

// Goals (/goal, like Claude Code's): after each turn, a separate request
// judges from the conversation whether the user's condition is met.

// The verdicts of a goal check.
const (
	GoalMet        = "met"
	GoalNotMet     = "not_met"
	GoalImpossible = "impossible"
	// GoalUnclear is a check without an answer that can be read: the model
	// thought until the limit, or called a tool instead. It says nothing
	// about the goal, so the goal pauses rather than keep the model working.
	GoalUnclear = "unclear"
)

// goalCheckTokens limits the check's reply. On a model that thinks, thinking
// counts toward it, and the check has the effort of the conversation: asking
// for less effort would cost the prompt cache of the whole conversation.
const goalCheckTokens = 16000

// GoalVerdict is the answer of a goal check.
type GoalVerdict struct {
	Verdict string // GoalMet, GoalNotMet or GoalImpossible
	Reason  string
	Usage   *provider.Usage
}

// CheckGoal asks the model whether the conversation shows condition met.
// The request repeats the conversation's own system prompt and tools, so a
// server's prompt cache covers all but the question, and its answer stays
// out of the conversation. An answer that can't be read is GoalUnclear.
func (s *Session) CheckGoal(ctx context.Context, condition string) (GoalVerdict, error) {
	p, req := s.request(append(slices.Clip(s.History()), provider.TextMessage(provider.RoleUser, prompt.GoalCheck(condition))))
	req.MaxTokens = goalCheckTokens
	stream, err := provider.StreamRetry(ctx, p, req, nil)
	if err != nil {
		return GoalVerdict{}, err
	}
	defer stream.Close()
	var text strings.Builder
	var v GoalVerdict
	defer func() { s.spent(p, v.Usage) }()
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return GoalVerdict{}, err
		}
		switch e := ev.(type) {
		case provider.TextDelta:
			text.WriteString(e.Text)
		case provider.Usage:
			v.Usage = &e
		}
	}
	v.Verdict, v.Reason = parseVerdict(s.redact(text.String()))
	return v, nil
}

var jsonObject = regexp.MustCompile(`(?s)\{.*?\}`)

// parseVerdict reads a goal check's answer: the first JSON object with a
// verdict in it, or else a verdict word in the text.
func parseVerdict(answer string) (verdict, reason string) {
	for _, obj := range jsonObject.FindAllString(answer, -1) {
		var v struct{ Verdict, Reason string }
		if json.Unmarshal([]byte(obj), &v) != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(v.Verdict)) {
		case "met", "achieved", "done", "yes":
			return GoalMet, strings.TrimSpace(v.Reason)
		case "not_met", "not met", "no", "not yet":
			return GoalNotMet, strings.TrimSpace(v.Reason)
		case "impossible":
			return GoalImpossible, strings.TrimSpace(v.Reason)
		}
	}
	lower := strings.ToLower(answer)
	switch {
	case strings.Contains(lower, "impossible"):
		return GoalImpossible, firstSentence(answer)
	case strings.Contains(lower, "not_met"), strings.Contains(lower, "not met"):
		return GoalNotMet, firstSentence(answer)
	case strings.Contains(lower, `"met"`):
		return GoalMet, firstSentence(answer)
	}
	return GoalUnclear, "the check's answer couldn't be read"
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ".\n"); i > 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
