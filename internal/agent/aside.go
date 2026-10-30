package agent

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"

	"debbie/internal/prompt"
	"debbie/internal/provider"
)

// MaxAsides is how many earlier side exchanges a side question carries.
const MaxAsides = 20

// Aside is a side question (/btw) and its answer.
type Aside struct {
	Question, Answer string
}

// ErrAsideTool is returned when the model answered a side question only by
// calling a tool, which side questions can't use.
var ErrAsideTool = errors.New("the model tried to use a tool, which a side question can't; ask in the conversation instead")

// Ask answers a side question about the conversation without it joining
// the conversation, so it can run while a turn does. The request repeats
// the conversation as the last finished turn left it, with the same system
// prompt and tools, so a server's prompt cache covers it; then the earlier
// side exchanges, newest last; then the question. Tool calls in the answer
// are ignored. onText, if not nil, gets the answer as it streams.
func (s *Session) Ask(ctx context.Context, question string, earlier []Aside, onText func(string)) (string, error) {
	msgs := slices.Clip(s.History())
	if len(earlier) > MaxAsides {
		earlier = earlier[len(earlier)-MaxAsides:]
	}
	for _, a := range earlier {
		msgs = append(msgs, provider.TextMessage(provider.RoleUser, prompt.Aside(a.Question)),
			provider.TextMessage(provider.RoleAssistant, a.Answer))
	}
	msgs = append(msgs, provider.TextMessage(provider.RoleUser, prompt.Aside(question)))
	p, req := s.request(msgs)
	stream, err := provider.StreamRetry(ctx, p, req, nil)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var text strings.Builder
	called := false
	var usage *provider.Usage
	defer func() { s.spent(p, usage) }()
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return s.redact(text.String()), err
		}
		switch e := ev.(type) {
		case provider.TextDelta:
			t := s.redact(e.Text)
			text.WriteString(t)
			if onText != nil {
				onText(t)
			}
		case provider.ToolCallStart:
			called = true
		case provider.Usage:
			usage = &e
		}
	}
	answer := strings.TrimSpace(text.String())
	if answer == "" && called {
		return "", ErrAsideTool
	}
	return answer, nil
}
