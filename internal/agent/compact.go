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
	compactRounds    = 3      // rounds of calls a turn lets pass after compacting
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
	compacted, res, err := s.compactMessages(ctx, s.History(), focus, contextWindow, progress)
	if err != nil {
		return res, err
	}
	s.setHistory(compacted, true)
	s.afterCompact()
	return res, nil
}

// afterCompact forgets what the summary took the place of.
func (s *Session) afterCompact() {
	clear(s.callIDs)
	s.mu.Lock()
	s.pushSaid, s.noteSaid = false, "" // the notes went with the conversation
	s.mu.Unlock()
}

// compactMessages returns the summary that replaces history.
func (s *Session) compactMessages(ctx context.Context, history []provider.Message, focus string, contextWindow int, progress func(chars int)) ([]provider.Message, CompactResult, error) {
	if len(history) < 2 {
		return nil, CompactResult{}, ErrNothingToCompact
	}
	before := EstimateTokens(history)
	msgs := history
	if contextWindow > 0 && before+compactMaxOutput+compactMargin > contextWindow {
		msgs = slim(msgs)
	}
	maxOut := compactMaxOutput
	if contextWindow > 0 {
		used := EstimateTokens(msgs) + s.compactOverhead(focus)/charsPerToken
		maxOut = max(1, min(compactMaxOutput, contextWindow-used-compactMargin))
	}
	summary, stop, err := s.summarize(ctx, msgs, prompt.CompressionRequest(focus), maxOut, false, progress)
	if err == nil && summary == "" && stop != provider.StopRefusal {
		// A long conversation can fill the reply with the drafting before
		// the summary starts; asked again, the model writes the summary
		// alone.
		summary, stop, err = s.summarize(ctx, msgs, prompt.CompressionRequest(focus)+"\n\n"+prompt.CompressionRetry(), maxOut, true, progress)
	}
	switch {
	case err != nil:
		return nil, CompactResult{}, err
	case summary != "":
	case stop == provider.StopRefusal:
		return nil, CompactResult{}, errors.New("the model declined to summarize the conversation; it is unchanged")
	case stop == provider.StopMaxTokens:
		return nil, CompactResult{}, fmt.Errorf("the model's reply reached its output limit (%d tokens) before the summary, twice; the conversation is unchanged", maxOut)
	default:
		return nil, CompactResult{}, errors.New("the model returned no summary, twice; the conversation is unchanged")
	}
	compacted := []provider.Message{
		provider.TextMessage(provider.RoleUser, prompt.CompactedSummary(summary)),
		provider.TextMessage(provider.RoleAssistant, prompt.CompactionAck()),
	}
	if EstimateTokens(compacted) >= before {
		return nil, CompactResult{Before: before, After: before, Messages: len(history)}, ErrNotSmaller
	}
	return compacted, CompactResult{Before: before, After: EstimateTokens(compacted), Messages: len(history)}, nil
}

// compactOverhead is the size, in characters, of what a summary request
// carries besides the conversation.
func (s *Session) compactOverhead(focus string) int {
	n := len(prompt.Compression()) + len(prompt.CompressionRequest(focus))
	p, req := s.request(nil)
	if cachesPrefix(p) {
		n += len(req.System)
		for _, t := range req.Tools {
			n += len(t.Name) + len(t.Description) + len(t.Schema)
		}
	}
	return n
}

// cachesPrefix reports whether a side request should repeat the
// conversation's prefix, for the provider's prompt cache.
func cachesPrefix(p provider.Provider) bool {
	c, ok := p.(provider.PrefixCacher)
	return ok && c.CachesPrefix()
}

// CompactWatcher is a Host that shows a compaction made while a turn runs.
type CompactWatcher interface {
	// CompactStart says one started; Compacted always follows it.
	CompactStart()
	Compacted(res CompactResult, err error)
}

// SetAutoCompact says at what context size, in tokens, a turn under way
// compacts the conversation (0 never), and the context window's size.
func (s *Session) SetAutoCompact(at, contextWindow int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compactAt, s.compactWindow = at, contextWindow
}

// compactIfFull compacts the conversation between rounds of calls when the
// next request would come near the limit: a long turn would otherwise run
// out of context before it ends.
//
// The summary covers what came before the last round of calls. That round,
// which the model hasn't seen the results of, goes with the message that
// tells it to carry on, word for word: the results themselves, and what came
// with them, such as a message from the user or an agent's report. That way
// nothing it is about to act on depends on a summary.
//
// A failure leaves the conversation as it is, and isn't tried again in this
// turn: the next request may still fit.
func (t *turn) compactIfFull(ctx context.Context, u *provider.Usage) {
	s := t.s
	s.mu.Lock()
	at, window := s.compactAt, s.compactWindow
	s.mu.Unlock()
	n := len(t.msgs)
	if at <= 0 || t.compactFailed || n < 4 {
		return
	}
	// A limit so low that the system prompt and a round or two reach it
	// shouldn't cost a summary every round.
	if t.compactWait > 0 {
		t.compactWait--
		return
	}
	used := EstimateTokens(t.msgs)
	if u != nil {
		// The last reply's real count, and the results it didn't include.
		used = u.Input + u.Output + EstimateTokens(t.msgs[n-1:])
	}
	if used < at {
		return
	}
	w, _ := t.host.(CompactWatcher)
	if w != nil {
		w.CompactStart()
	}
	compacted, res, err := s.compactMessages(ctx, t.msgs[:n-2], "", window, nil)
	if err != nil {
		t.compactFailed = true
		if w != nil {
			w.Compacted(res, err)
		}
		return
	}
	s.afterCompact()
	carryOn := s.userMessage(prompt.CompressionContinue(lastRound(t.msgs[n-2], t.msgs[n-1])))
	carryOn.Parts[0].Hidden = true // the user wrote none of it
	t.msgs = append(compacted, carryOn)
	t.compactWait = compactRounds
	s.setHistory(t.msgs, true)
	if w != nil {
		w.Compacted(res, nil)
	}
}

// lastRound writes out a reply and the results of its calls as text.
func lastRound(reply, results provider.Message) string {
	byCall := map[string]*provider.ToolResult{}
	for _, p := range results.Parts {
		if p.ToolResult != nil {
			byCall[p.ToolResult.CallID] = p.ToolResult
		}
	}
	var b strings.Builder
	if text := strings.TrimSpace(reply.Text()); text != "" {
		b.WriteString("You wrote:\n" + text + "\n\n")
	}
	for _, p := range reply.Parts {
		c := p.ToolCall
		if c == nil {
			continue
		}
		fmt.Fprintf(&b, "You called %s with %s, and got:\n", c.Name, c.Args)
		if r := byCall[c.ID]; r != nil {
			if r.IsError {
				b.WriteString("Error: ")
			}
			b.WriteString(r.Content + "\n")
			if len(r.Images) > 0 {
				b.WriteString("[An image came with it, which was left out; read or take it again if you need it.]\n")
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// summarize asks the model once for the summary of msgs, the request as
// the last message, and returns what follows its drafting, and why the
// reply ended. The reasoning effort is low where the session sets one: a
// summary needs no deep thought, and on a model that thinks, thinking
// shares the output limit with the summary.
//
// A provider with a prefix cache gets the conversation's own system prompt,
// tools and effort, and the instructions in the message: the summary then
// reads the whole conversation from the cache, which is far cheaper. Only a
// retry lowers the effort there, since that costs the cache.
func (s *Session) summarize(ctx context.Context, msgs []provider.Message, request string, maxOut int, retry bool, progress func(chars int)) (string, provider.StopReason, error) {
	p, req := s.request(nil)
	cached := cachesPrefix(p)
	if cached {
		request = prompt.CompressionInline() + "\n\n" + request
	} else {
		req.System, req.Tools = prompt.Compression(), nil
	}
	req.Messages = append(msgs[:len(msgs):len(msgs)], provider.TextMessage(provider.RoleUser, request))
	req.MaxTokens = maxOut
	if req.Effort != effort.Default && (!cached || retry) {
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
	summary := stripAnalysis(text.String())
	switch {
	case stop == provider.StopToolUse, stop == provider.StopMaxTokens, stop == provider.StopFiltered:
		summary = "" // cut short, or it went on working: no summary to replace a conversation with
	case cached && !strings.Contains(summary, "<state_snapshot>"):
		summary = "" // with its tools at hand, it answered something else
	}
	return summary, stop, nil
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
