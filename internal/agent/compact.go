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
	compactRounds    = 3      // rounds of calls a turn lets pass after compacting, or after failing to
	compactTries     = 2      // failed compactions after which a turn stops trying

	// A turn compacted while it runs keeps its latest rounds of calls word
	// for word, beside the summary: as many as fit in a share of the size
	// that set the compaction off, so the model goes on from what it just
	// read instead of reading it again.
	tailShare = 5      // one part in this many
	tailMax   = 40_000 // tokens, at most

	// thoughtKeep is how much of a reply's reasoning, from its end, the
	// summarizer gets to read when the provider doesn't keep reasoning in
	// the conversation: where a model that thinks reaches its conclusions.
	thoughtKeep = 2_000
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

func EstimateTokens(msgs []provider.Message) int { return estimate(msgs, true) }

// Size guesses the size of the conversation in tokens as the session's
// provider gets it: without the reasoning, if it doesn't take that back.
func (s *Session) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return estimate(s.history, keepsReasoning(s.provider))
}

// estimate guesses the size of messages in tokens; reasoning says whether
// the replies' reasoning counts, which a provider that doesn't take it
// back never sees again.
func estimate(msgs []provider.Message, reasoning bool) int {
	chars := 0
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Kind == provider.PartReasoning && !reasoning {
				continue
			}
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
	p, _ := s.request(nil)
	// Reasoning the provider never gets back takes no room in the request:
	// counted, it left a long conversation of a model that thinks a lot
	// with next to no room for its summary.
	keeps := keepsReasoning(p)
	before := estimate(history, keeps)
	over := func(msgs []provider.Message) bool {
		return contextWindow > 0 && estimate(msgs, keeps)+compactMaxOutput+compactMargin > contextWindow
	}
	msgs := history
	if !cachesPrefix(p) {
		// What the model concluded is in its reasoning, which the summarizer
		// may not see: a provider that doesn't keep reasoning never sends it,
		// and one that does may send only the current turn's, which the
		// request for the summary ends. Without it the summarizer would have
		// to work everything out again from the tool results, in one pass.
		msgs = withThoughts(history)
	}
	if keeps && over(msgs) {
		msgs = history // the reasoning itself may go along, so the excerpts go first
	}
	if over(msgs) {
		msgs = slim(msgs)
	}
	if over(msgs) {
		msgs = slim(history)
	}
	maxOut := compactMaxOutput
	if contextWindow > 0 {
		used := estimate(msgs, keeps) + s.compactOverhead(focus)/charsPerToken
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

// withThoughts returns msgs with the end of each reply's reasoning as text
// at the start of the reply, for the summarizer to read whatever the
// provider does with the reasoning itself, which stays. A reply without
// reasoning stays as it is, and so does reasoning that is a provider's own
// block.
func withThoughts(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if text := thoughtEnd(m); text != "" {
			excerpt := provider.Part{Kind: provider.PartText, Text: prompt.CompressionThought(text) + "\n\n"}
			out[i].Parts = append([]provider.Part{excerpt}, m.Parts...)
		}
	}
	return out
}

// thoughtEnd is the end of a reply's reasoning, where a model that thinks
// reaches its conclusions: its last thoughtKeep characters, "" when the
// message has none that is plain text.
func thoughtEnd(m provider.Message) string {
	if m.Role != provider.RoleAssistant {
		return ""
	}
	var thought strings.Builder
	for _, p := range m.Parts {
		if p.Kind == provider.PartReasoning && p.Opaque == nil {
			thought.WriteString(p.Text)
		}
	}
	text := strings.TrimSpace(thought.String())
	if len(text) > thoughtKeep {
		text = "… " + text[runeStart(text, len(text)-thoughtKeep):]
	}
	return text
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
// The summary covers the whole conversation. The turn's latest rounds of
// calls go with the message that tells the model to carry on, word for
// word (see recentRounds): the last one, which it hasn't seen the results
// of, with what came along, such as a message from the user or an agent's
// report, and as many before it as fit. That way nothing it is about to
// act on depends on a summary, and it doesn't start by reading again what
// it had just read.
//
// A failure leaves the conversation as it is: the next request may still
// fit. It is tried once more, some rounds later.
func (t *turn) compactIfFull(ctx context.Context, r reply) {
	s := t.s
	s.mu.Lock()
	at, window, p := s.compactAt, s.compactWindow, s.provider
	s.mu.Unlock()
	n := len(t.msgs)
	if at <= 0 || t.compactFails >= compactTries || n < 4 {
		return
	}
	// A limit so low that the system prompt and a round or two reach it
	// shouldn't cost a summary every round.
	if t.compactWait > 0 {
		t.compactWait--
		return
	}
	used := estimate(t.msgs, keepsReasoning(p))
	if r.usage != nil {
		// What the last reply's real count carries on, and the results it
		// didn't include.
		used = r.carried + EstimateTokens(t.msgs[n-1:])
	}
	if used < at {
		return
	}
	w, _ := t.host.(CompactWatcher)
	if w != nil {
		w.CompactStart()
	}
	compacted, res, err := s.compactMessages(ctx, t.msgs, "", window, nil)
	if err != nil {
		t.compactFails++
		t.compactWait = compactRounds
		if w != nil {
			w.Compacted(res, err)
		}
		return
	}
	s.afterCompact()
	tail := t.msgs[recentRounds(t.msgs, min(at/tailShare, tailMax)):]
	carryOn := s.userMessage(prompt.CompressionContinue(rounds(tail)))
	carryOn.Parts[0].Hidden = true // the user wrote none of it
	t.msgs = append(compacted, carryOn)
	t.compactWait = compactRounds
	// What it read went with the conversation: reading it again is no stall.
	t.stall = 0
	clear(t.seen)
	s.setHistory(t.msgs, true)
	res.After = EstimateTokens(t.msgs)
	if w != nil {
		w.Compacted(res, nil)
	}
}

// recentRounds returns where the latest rounds of calls start in msgs, the
// conversation of a turn that just got a round's results: as many whole
// rounds, a reply with calls and the results of them, as take at most
// budget tokens written out, and the last one whatever it takes.
func recentRounds(msgs []provider.Message, budget int) int {
	from, size := len(msgs)-2, 0
	for i := len(msgs) - 2; i >= 1 && isRound(msgs[i], msgs[i+1]); i -= 2 {
		size += estimate(msgs[i:i+2], false)
		if size > budget && i < len(msgs)-2 {
			break
		}
		from = i
	}
	return from
}

// isRound reports whether two messages are a reply that made calls and the
// results of them.
func isRound(reply, results provider.Message) bool {
	if reply.Role != provider.RoleAssistant || results.Role != provider.RoleUser {
		return false
	}
	called, answered := false, false
	for _, p := range reply.Parts {
		called = called || p.ToolCall != nil
	}
	for _, p := range results.Parts {
		answered = answered || p.ToolResult != nil
	}
	return called && answered
}

// rounds writes out rounds of calls, each a reply and its results, as text.
func rounds(msgs []provider.Message) string {
	var b strings.Builder
	n := len(msgs) / 2
	for i := 0; i+1 < len(msgs); i += 2 {
		if n > 1 {
			fmt.Fprintf(&b, "Round %d of %d:\n", i/2+1, n)
		}
		b.WriteString(lastRound(msgs[i], msgs[i+1]) + "\n\n")
	}
	return strings.TrimSpace(b.String())
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
	// What it concluded before it made the calls: a model that reads its
	// earlier reasoning would otherwise go on without it.
	if thought := thoughtEnd(reply); thought != "" {
		b.WriteString("Your reasoning ended:\n" + thought + "\n\n")
	}
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
