package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"debbie/internal/effort"
	"debbie/internal/prompt"
	"debbie/internal/provider"
)

// Compaction (SPEC §7): when the conversation nears the context window, or
// the user asks with /compact, the model summarizes it, and the summary
// replaces it. The approach follows Qwen Code's compression service.

const (
	// DefaultAutoCompact is the context size, in tokens, at which the UI
	// compacts the conversation unless the config says otherwise.
	DefaultAutoCompact = 180_000

	compactMaxOutput = 32_000 // the most a summary may take, its drafting and any thinking included
	compactMargin    = 1_024  // room left for the estimate being off
	charsPerToken    = 4      // the rough size of a token, for estimates
	slimKeep         = 1_500  // characters kept at each end of a long tool result
)

// ErrNothingToCompact is returned for a conversation too short to compact.
var ErrNothingToCompact = errors.New("there is nothing to compact yet")

// ErrNotSmaller is returned when the summary came out no smaller than the
// conversation it would replace, which then stays (Qwen Code's inflation
// guard).
var ErrNotSmaller = errors.New("the summary wasn't smaller than the conversation, so the conversation stays as it is")

// CompactResult describes a compaction.
type CompactResult struct {
	Before, After int // the conversation's estimated size, in tokens
	Messages      int // how many messages the summary replaced
}

// EstimateTokens guesses the size of messages in tokens, from their length.
// imageTokens is about what an image costs a vision model: a screenshot of
// 1280 by 800 pixels is a thousand to two thousand tokens.
const imageTokens = 1500

func EstimateTokens(msgs []provider.Message) int {
	chars := 0
	for _, m := range msgs {
		for _, p := range m.Parts {
			chars += len(p.Text)
			if p.ToolCall != nil {
				chars += len(p.ToolCall.Name) + len(p.ToolCall.Args)
			}
			if p.ToolResult != nil {
				chars += len(p.ToolResult.Content) + len(p.ToolResult.Images)*imageTokens*charsPerToken
			}
		}
	}
	return chars / charsPerToken
}

// AutoCompactAt is the context size, in tokens, at which a conversation is
// compacted: threshold, but at most 85% of a known context window, so the
// summary still fits. 0 means never.
func AutoCompactAt(threshold, contextWindow int) int {
	if threshold <= 0 {
		return 0
	}
	if contextWindow > 0 {
		return min(threshold, contextWindow*85/100)
	}
	return threshold
}

// Compact asks the model to summarize the conversation and replaces the
// conversation with the summary. focus, if not empty, tells it what to
// keep in particular. contextWindow, when known, limits what is sent:
// long tool results are shortened if the conversation wouldn't fit with
// room for the summary. progress, if not nil, learns the summary's length
// as it arrives. The conversation stays as it was if anything fails.
func (s *Session) Compact(ctx context.Context, focus string, contextWindow int, progress func(chars int)) (CompactResult, error) {
	history := s.History()
	if len(history) < 2 {
		return CompactResult{}, ErrNothingToCompact
	}
	before := EstimateTokens(history)
	msgs := history
	if contextWindow > 0 && before+compactMaxOutput+compactMargin > contextWindow {
		msgs = slim(msgs)
	}
	maxOut := compactMaxOutput
	if contextWindow > 0 {
		used := EstimateTokens(msgs) + (len(prompt.Compression())+len(prompt.CompressionRequest(focus)))/charsPerToken
		maxOut = max(1, min(compactMaxOutput, contextWindow-used-compactMargin))
	}
	summary, stop, err := s.summarize(ctx, msgs, prompt.CompressionRequest(focus), maxOut, progress)
	if err == nil && summary == "" && stop != provider.StopRefusal {
		// A long conversation can fill the reply with the drafting before
		// the summary starts; asked again, the model writes the summary
		// alone.
		summary, stop, err = s.summarize(ctx, msgs, prompt.CompressionRequest(focus)+"\n\n"+prompt.CompressionRetry(), maxOut, progress)
	}
	switch {
	case err != nil:
		return CompactResult{}, err
	case summary != "":
	case stop == provider.StopRefusal:
		return CompactResult{}, errors.New("the model declined to summarize the conversation; it is unchanged")
	case stop == provider.StopMaxTokens:
		return CompactResult{}, fmt.Errorf("the model's reply reached its output limit (%d tokens) before the summary, twice; the conversation is unchanged", maxOut)
	default:
		return CompactResult{}, errors.New("the model returned no summary, twice; the conversation is unchanged")
	}
	compacted := []provider.Message{
		provider.TextMessage(provider.RoleUser, prompt.CompactedSummary(summary)),
		provider.TextMessage(provider.RoleAssistant, prompt.CompactionAck()),
	}
	if EstimateTokens(compacted) >= before {
		return CompactResult{Before: before, After: before, Messages: len(history)}, ErrNotSmaller
	}
	s.setHistory(compacted, true)
	clear(s.callIDs)
	return CompactResult{Before: before, After: EstimateTokens(compacted), Messages: len(history)}, nil
}

// summarize asks the model once for the summary of msgs, the request as
// the last message, and returns what follows its drafting, and why the
// reply ended. The reasoning effort is low where the session sets one: a
// summary needs no deep thought, and on a model that thinks, thinking
// shares the output limit with the summary.
func (s *Session) summarize(ctx context.Context, msgs []provider.Message, request string, maxOut int, progress func(chars int)) (string, provider.StopReason, error) {
	msgs = append(msgs[:len(msgs):len(msgs)], provider.TextMessage(provider.RoleUser, request))
	p, req := s.request(msgs)
	req.System, req.Tools, req.MaxTokens = prompt.Compression(), nil, maxOut
	if req.Effort != effort.Default {
		req.Effort = effort.Low
	}
	stream, err := provider.StreamRetry(ctx, p, req, nil)
	if err != nil {
		return "", "", err
	}
	defer stream.Close()
	var text strings.Builder
	stop := provider.StopEnd
	var usage *provider.Usage
	defer func() { s.spent(p, usage) }()
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", err
		}
		switch e := ev.(type) {
		case provider.TextDelta:
			text.WriteString(e.Text)
			if progress != nil {
				progress(text.Len())
			}
		case provider.Usage:
			usage = &e
		case provider.Done:
			stop = e.StopReason
		}
	}
	return stripAnalysis(text.String()), stop, nil
}

var (
	analysisBlock    = regexp.MustCompile(`(?s)<analysis>.*?</analysis>\s*`)
	analysisUnclosed = regexp.MustCompile(`(?s)<analysis>.*$`)
)

// stripAnalysis removes the summarizer's drafting: its <analysis> blocks,
// and one left open when it ran out of room.
func stripAnalysis(s string) string {
	s = analysisBlock.ReplaceAllString(s, "")
	s = analysisUnclosed.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// slim shortens long tool results, keeping their start and end, so a
// conversation near the end of the context window fits in the request for
// its summary.
func slim(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		copied := false // the message's parts are still the conversation's
		for j, p := range m.Parts {
			if p.ToolResult == nil || len(p.ToolResult.Content) <= 4*slimKeep {
				continue
			}
			if !copied {
				out[i].Parts = append([]provider.Part(nil), m.Parts...)
				copied = true
			}
			c := p.ToolResult.Content
			head, tail := runeStart(c, slimKeep), runeStart(c, len(c)-slimKeep)
			short := *p.ToolResult
			short.Content = fmt.Sprintf("%s\n[… %d characters left out for the summary …]\n%s", c[:head], tail-head, c[tail:])
			out[i].Parts[j].ToolResult = &short
		}
	}
	return out
}

// runeStart moves i back to the start of the character it falls in.
func runeStart(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}
