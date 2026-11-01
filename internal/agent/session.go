// Package agent runs conversations with a model: the agent loop of SPEC §7.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"debbie/internal/config"
	"debbie/internal/effort"
	"debbie/internal/prompt"
	"debbie/internal/provider"
	"debbie/internal/tools"
)

// Limits of the agent loop (SPEC §7).
const (
	DefaultMaxTurns = 100 // model requests per message
	maxFailures     = 3   // invalid calls of one kind in a row
	maxRepeats      = 3   // identical calls, or texts, in a row; one more stops the turn
)

// Session is one conversation with a provider.
type Session struct {
	// mu guards what the user may change, or read, while a turn or a side
	// question runs on another goroutine: switch the provider, the mode or
	// the effort.
	mu        sync.Mutex
	provider  provider.Provider
	system    string
	maxTokens int
	effort    effort.Level
	budget    int // tokens of reasoning per reply; 0 leaves it to the server
	history   []provider.Message
	mode      string
	web       string // the web access mode (SPEC §19)
	tainted   bool   // web content is in the conversation (SPEC §19.5)

	tools    *tools.Registry // nil for a plain chat
	env      *tools.Env
	allowed  map[string]bool // what the user allowed for the session, by session key
	maxTurns int
	redact   func(string) string
	callIDs  map[string]bool // tool call IDs in use
	recorder Recorder        // keeps the conversation as it changes; nil for none
	ledger   *Ledger         // adds up what the session spends; nil for none

	interjections []string // the user's messages for the turn under way; guarded by mu
}

// Interject passes a message from the user to the turn under way, which
// gets it with its next tool results. One the turn ends without is still
// pending (see takeInterjections).
func (s *Session) Interject(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interjections = append(s.interjections, text)
}

// takeInterjections returns the pending messages from the user and
// forgets them.
func (s *Session) takeInterjections() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.interjections
	s.interjections = nil
	return out
}

// userNote is how a message the user sent while the model worked reaches
// it.
func userNote(text string) string {
	return "[A message from the user, sent while you were working: " + text + "]"
}

// New starts a conversation. maxTokens limits each reply; 0 leaves the
// limit to the server. Without SetTools it is a plain chat.
func New(p provider.Provider, system string, maxTokens int) *Session {
	return &Session{
		provider:  p,
		system:    system,
		maxTokens: maxTokens,
		mode:      config.ApprovalDefault,
		allowed:   map[string]bool{},
		maxTurns:  DefaultMaxTurns,
		redact:    func(s string) string { return s },
		callIDs:   map[string]bool{},
	}
}

// SetProvider switches the model the session talks to. The conversation
// carries on.
func (s *Session) SetProvider(p provider.Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider = p
}

// SetSystem replaces the system prompt.
func (s *Session) SetSystem(system string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.system = system
}

// SetMaxTokens changes the limit on each reply; 0 leaves it to the server.
func (s *Session) SetMaxTokens(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxTokens = n
}

// SetTools gives the model tools to call.
func (s *Session) SetTools(r *tools.Registry, env *tools.Env) { s.tools, s.env = r, env }

// Tools returns the session's tools, nil for a plain chat.
func (s *Session) Tools() *tools.Registry { return s.tools }

// Mode returns the approval mode (SPEC §9).
func (s *Session) Mode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// SetMode changes the approval mode. Confirming yolo is the caller's job.
func (s *Session) SetMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
}

// Web returns the web access mode (SPEC §19): config.WebOff, WebSafe or
// WebYolo.
func (s *Session) Web() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.web == "" {
		return config.WebOff
	}
	return s.web
}

// SetWeb changes the web access mode. Confirming yolo, and the tools that
// come and go with web access, are the caller's job.
func (s *Session) SetWeb(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.web = mode
}

// Tainted reports whether web content is in the conversation (SPEC
// §19.5): then, in SAFE web mode, changes and commands ask in every
// approval mode. Reset clears it.
func (s *Session) Tainted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tainted
}

func (s *Session) taint() {
	s.mu.Lock()
	s.tainted = true
	s.mu.Unlock()
	if s.recorder != nil {
		s.recorder.Taint()
	}
}

// taintAsks reports whether web content in the conversation makes a call
// of this kind ask.
func (s *Session) taintAsks(k tools.Kind) bool {
	return (k == tools.Edit || k == tools.Execute) && s.Web() == config.WebSafe && s.Tainted()
}

// SetMaxTurns changes how many model requests one message may lead to.
func (s *Session) SetMaxTurns(n int) { s.maxTurns = n }

// SetRedactor sets the function that removes secrets from tool output.
func (s *Session) SetRedactor(redact func(string) string) { s.redact = redact }

// Effort returns the reasoning effort requests ask for.
func (s *Session) Effort() effort.Level {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effort
}

// SetEffort changes the reasoning effort for the rest of the session.
func (s *Session) SetEffort(l effort.Level) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.effort = l
}

// ReasoningBudget returns the tokens of reasoning requests allow per
// reply; 0 leaves it to the server.
func (s *Session) ReasoningBudget() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.budget
}

// SetReasoningBudget caps the reasoning per reply for the rest of the
// session; 0 leaves it to the server.
func (s *Session) SetReasoningBudget(tokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.budget = tokens
}

// Recorder keeps the conversation as it changes, to resume it later
// (sessions.Recorder, SPEC §12).
type Recorder interface {
	// Record learns the history after a change: replaced is false when it
	// only grew since the last call, true when it was started over.
	Record(history []provider.Message, replaced bool)
	// Taint notes that web content joined the conversation.
	Taint()
}

// SetRecorder sets what keeps the conversation as it changes.
func (s *Session) SetRecorder(r Recorder) { s.recorder = r }

// Resume replaces the conversation with a saved one, which is already
// recorded; web content in it taints the session as it did then.
func (s *Session) Resume(history []provider.Message, tainted bool) {
	s.mu.Lock()
	s.history, s.tainted = history, tainted
	s.mu.Unlock()
	clear(s.callIDs)
	for _, m := range history {
		for _, p := range m.Parts {
			if p.ToolCall != nil {
				s.callIDs[p.ToolCall.ID] = true
			}
		}
	}
}

// Reset forgets the conversation, and with it any web content. What the
// user allowed for the session stays allowed.
func (s *Session) Reset() {
	s.setHistory(nil, true)
	clear(s.callIDs)
	s.mu.Lock()
	s.tainted = false
	s.mu.Unlock()
}

// History returns the conversation so far. The messages are never changed
// in place, so the slice stays as it is.
func (s *Session) History() []provider.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.history
}

// setHistory changes the conversation and records it: replaced is false
// when msgs only adds to it.
func (s *Session) setHistory(msgs []provider.Message, replaced bool) {
	s.mu.Lock()
	s.history = msgs
	s.mu.Unlock()
	s.record(msgs, replaced)
}

func (s *Session) record(msgs []provider.Message, replaced bool) {
	if s.recorder != nil {
		s.recorder.Record(msgs, replaced)
	}
}

// request continues the conversation with msgs: the provider to send it
// to, with the session's system prompt, its tools as the mode allows them,
// and its effort.
func (s *Session) request(msgs []provider.Message) (provider.Provider, provider.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req := provider.Request{System: s.system, Messages: msgs, MaxTokens: s.maxTokens, Effort: s.effort, ReasoningBudget: s.budget}
	if s.tools != nil {
		switch s.mode {
		case config.ApprovalPlan:
			req.System += "\n\n" + prompt.PlanMode()
			req.Tools = s.tools.Specs(tools.Read)
		case config.ApprovalEditor:
			req.System += "\n\n" + prompt.EditorMode()
			req.Tools = s.tools.Specs(tools.Read, tools.Edit)
		default:
			req.Tools = s.tools.Specs()
		}
	}
	return s.provider, req
}

// Available reports whether tools of a kind may run in the current mode:
// plan mode only reads, and editor mode runs no commands.
func (s *Session) Available(k tools.Kind) bool {
	switch s.Mode() {
	case config.ApprovalPlan:
		return k == tools.Read
	case config.ApprovalEditor:
		return k != tools.Execute
	}
	return true
}

// Result describes one turn.
type Result struct {
	Text       string // the last reply's text
	Reasoning  string
	Usage      *provider.Usage // of the last request; nil when the server reported none
	StopReason provider.StopReason
	StopDetail string // more about the stop, such as why a model refused
	Steps      int    // requests sent
	Stopped    string // why the agent ended the turn early, if it did
}

// errRepeating ends a reply that keeps repeating itself.
var errRepeating = errors.New("the reply keeps repeating itself")

// Send runs one turn: it sends the user's message, shows the reply through
// host, runs the tool calls the reply makes and sends back their results,
// until a reply makes no calls. Everything the model said and did joins the
// history, even when the turn broke off, by cancellation or an error, so
// the user can ask the model to continue; the error is returned with the
// result. A turn in which the model said nothing at all leaves the history
// unchanged, so the user can simply send the message again.
func (s *Session) Send(ctx context.Context, text string, host Host) (Result, error) {
	t := &turn{s: s, host: host, msgs: append(slices.Clip(s.History()), provider.TextMessage(provider.RoleUser, text))}
	res, err := t.run(ctx)
	if t.produced {
		s.setHistory(t.msgs, false)
	}
	return res, err
}

// turn is the state of one Send.
type turn struct {
	s        *Session
	host     Host
	msgs     []provider.Message
	produced bool // the model has said or done something

	lastCall    string // the previous call, as callKey
	callRepeats int
	lastText    string // the previous reply's text, when it made calls
	textRepeats int
	failKind    string // the kind of the previous invalid calls
	failures    int
	notes       []string // loop warnings to send with the next tool results
}

func (t *turn) run(ctx context.Context) (Result, error) {
	var res Result
	for step := 1; ; step++ {
		if step > t.s.maxTurns {
			res.Stopped = fmt.Sprintf("the message led to %d model requests, the limit (max_turns in the config). "+
				"Nothing is lost: send \"continue\" to carry on", t.s.maxTurns)
			return res, nil
		}
		r, err := t.reply(ctx)
		res.Steps = step
		res.Text, res.Reasoning, res.StopReason, res.StopDetail = r.text, r.reasoning, r.stop, r.detail
		if r.usage != nil {
			res.Usage = r.usage
		}
		if r.stop == provider.StopRefusal && err == nil {
			// What a refused reply wrote isn't an answer, and none of its
			// calls may run: it stays out of the conversation.
			return res, nil
		}
		if err != nil {
			r.calls = nil // calls without results would break the next request
			t.add(r)
			if errors.Is(err, errRepeating) {
				res.Stopped = "the model kept repeating the same text"
				return res, nil
			}
			return res, err
		}
		t.add(r)
		if len(r.calls) == 0 {
			return res, nil
		}
		stop := t.checkText(r.text)
		results, stop := t.runCalls(ctx, r.calls, stop)
		if notes := t.s.takeInterjections(); len(notes) > 0 {
			last := results.Parts[len(results.Parts)-1].ToolResult
			for _, n := range notes {
				last.Content += "\n\n" + userNote(n)
			}
		}
		t.msgs = append(t.msgs, results)
		t.s.record(t.msgs, false) // saved as it goes, should the program end mid-turn
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if stop != "" {
			res.Stopped = stop
			return res, nil
		}
	}
}

// reply is one response of the model.
type reply struct {
	text, reasoning string
	// parts is what it thought, said and called, in the order it did:
	// providers such as Anthropic want their blocks back that way, thinking
	// blocks unchanged (SPEC §4.4). A call's part points into calls.
	parts  []provider.Part
	calls  []provider.ToolCall // complete ones only
	usage  *provider.Usage
	stop   provider.StopReason
	detail string
}

// add appends a reply to the turn's messages, if it says or does anything.
// Only the calls left in r.calls go with it: one taken out has no result.
func (t *turn) add(r reply) {
	if r.text == "" && len(r.calls) == 0 {
		return
	}
	keep := map[*provider.ToolCall]bool{}
	for i := range r.calls {
		keep[&r.calls[i]] = true
	}
	msg := provider.Message{Role: provider.RoleAssistant}
	for _, p := range r.parts {
		if p.Kind != provider.PartToolCall || keep[p.ToolCall] {
			msg.Parts = append(msg.Parts, p)
		}
	}
	t.msgs = append(t.msgs, msg)
	t.produced = true
	t.s.record(t.msgs, false)
}

// reply streams one response to the host and collects it.
func (t *turn) reply(ctx context.Context) (reply, error) {
	s := t.s
	t.host.ReplyStart()
	defer t.host.ReplyEnd()
	p, req := s.request(t.msgs)
	stream, err := provider.StreamRetry(ctx, p, req, func(r provider.Retrying) { t.host.Event(r) })
	if err != nil {
		return reply{}, err
	}
	defer stream.Close()

	var r reply
	defer func() { s.spent(p, r.usage) }()
	var text, reasoning strings.Builder
	var calls []provider.ToolCall
	index := map[string]int{} // call ID → position in calls
	ended := map[string]bool{}
	var chant chantDetector
	// The parts in order: text and reasoning as they run, and for a call
	// its position in calls (Text unused), resolved once the reply is in.
	type piece struct {
		part provider.Part
		call int // -1 for none
	}
	var pieces []piece
	var run strings.Builder
	runKind := provider.PartKind(-1)
	flush := func() {
		if runKind >= 0 && run.Len() > 0 {
			pieces = append(pieces, piece{part: provider.Part{Kind: runKind, Text: run.String()}, call: -1})
		}
		run.Reset()
		runKind = -1
	}
	grow := func(kind provider.PartKind, s string) {
		if runKind != kind {
			flush()
			runKind = kind
		}
		run.WriteString(s)
	}
	finish := func() {
		flush()
		r.text, r.reasoning = text.String(), reasoning.String()
		at := map[int]int{} // position in calls → in r.calls
		for i, c := range calls {
			if ended[c.ID] {
				at[i] = len(r.calls)
				r.calls = append(r.calls, c)
			}
		}
		t.s.assignIDs(r.calls)
		for _, pc := range pieces {
			if pc.call < 0 {
				r.parts = append(r.parts, pc.part)
			} else if j, ok := at[pc.call]; ok {
				r.parts = append(r.parts, provider.Part{Kind: provider.PartToolCall, ToolCall: &r.calls[j]})
			}
		}
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			finish()
			return r, err
		}
		t.host.Event(ev)
		switch e := ev.(type) {
		case provider.TextDelta:
			text.WriteString(e.Text)
			grow(provider.PartText, e.Text)
			if chant.text(e.Text) {
				finish()
				return r, errRepeating
			}
		case provider.TextEnd:
			flush()
		case provider.ReasoningDelta:
			reasoning.WriteString(e.Text)
			grow(provider.PartReasoning, e.Text)
			if chant.reasoning(e.Text) {
				finish()
				return r, errRepeating
			}
		case provider.ReasoningEnd:
			thought := ""
			if runKind == provider.PartReasoning {
				thought = run.String()
				run.Reset()
				runKind = -1
			}
			flush()
			pieces = append(pieces, piece{part: provider.Part{Kind: provider.PartReasoning, Text: thought, Opaque: e.Opaque}, call: -1})
		case provider.ToolCallStart:
			flush()
			index[e.ID] = len(calls)
			pieces = append(pieces, piece{call: len(calls)})
			calls = append(calls, provider.ToolCall{ID: e.ID, Name: e.Name})
		case provider.ToolCallArgsDelta:
			if i, ok := index[e.ID]; ok {
				calls[i].Args = append(calls[i].Args, e.Args...)
			}
		case provider.ToolCallEnd:
			ended[e.ID] = true
		case provider.Usage:
			r.usage = &e
		case provider.Done:
			r.stop, r.detail = e.StopReason, e.Detail
		}
	}
}

// assignIDs gives calls without an ID, or with one already in use, a new
// one: results are matched to calls by ID.
func (s *Session) assignIDs(calls []provider.ToolCall) {
	for i := range calls {
		for n := len(s.callIDs); calls[i].ID == "" || s.callIDs[calls[i].ID]; n++ {
			calls[i].ID = fmt.Sprintf("call_%d", n)
		}
		s.callIDs[calls[i].ID] = true
	}
}

// checkText watches for a model that says the same thing before each round
// of calls. It returns a reason to stop the turn on the fourth time.
func (t *turn) checkText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if text == t.lastText {
		t.textRepeats++
	} else {
		t.lastText, t.textRepeats = text, 1
	}
	switch {
	case t.textRepeats > maxRepeats:
		return fmt.Sprintf("the model wrote the same text %d times in a row", t.textRepeats)
	case t.textRepeats == maxRepeats:
		t.notes = append(t.notes, fmt.Sprintf("[Note: you have written the same text %d times in a row. "+
			"You may be stuck in a loop: step back and try a different approach. Repeating it once more stops this turn.]", maxRepeats))
	}
	return ""
}

// outcome is what a tool call gives the model.
type outcome struct {
	content string
	isErr   bool
	images  []provider.Image
}

// runCalls runs a reply's tool calls in order and returns their results.
// Consecutive task calls run together, their subagents side by side (see
// runTogether). Every call gets a result, also those that don't run
// because the turn stops; stop is the reason, if it does.
func (t *turn) runCalls(ctx context.Context, calls []provider.ToolCall, stop string) (provider.Message, string) {
	msg := provider.Message{Role: provider.RoleUser}
	out := make([]outcome, len(calls))
	for i := 0; i < len(calls); {
		n := 1
		for calls[i].Name == "task" && i+n < len(calls) && calls[i+n].Name == "task" {
			n++
		}
		switch {
		case ctx.Err() != nil:
			out[i], n = outcome{content: "Not run: the user cancelled the turn.", isErr: true}, 1
		case stop != "":
			out[i], n = outcome{content: "Not run: the turn was stopped.", isErr: true}, 1
		case n > 1:
			stop = t.runTogether(ctx, calls[i:i+n], out[i:i+n])
		default:
			out[i], stop = t.runCall(ctx, calls[i])
		}
		i += n
	}
	for i, c := range calls {
		msg.Parts = append(msg.Parts, provider.Part{Kind: provider.PartToolResult,
			ToolResult: &provider.ToolResult{CallID: c.ID, Content: out[i].content, IsError: out[i].isErr, Images: out[i].images}})
	}
	if len(t.notes) > 0 {
		last := msg.Parts[len(msg.Parts)-1].ToolResult
		last.Content += "\n\n" + strings.Join(t.notes, "\n")
		t.notes = nil
	}
	return msg, stop
}

var failureNames = map[string]string{
	"unknown": "unknown tool",
	"blocked": "tool not available in this mode",
	"json":    "arguments that aren't valid JSON",
	"params":  "wrong parameters",
}

// runCall runs one tool call: it checks that the tool exists and the
// arguments fit, watches for loops, asks for approval, runs it and returns
// what the model gets back.
func (t *turn) runCall(ctx context.Context, c provider.ToolCall) (outcome, string) {
	p, content, isErr, stop := t.prepare(ctx, c)
	if p == nil {
		return outcome{content: content, isErr: isErr}, stop
	}
	t.host.ToolStart(p.run)
	return t.exec(ctx, p, nil), ""
}

// runTogether runs consecutive task calls at once: each is checked, and
// approved if need be, in order; then their subagents work side by side,
// as many at a time as the task tool lets run (subagents.max_parallel),
// the others waiting for a slot. hostMu keeps the calls' own reports to
// the host one at a time.
func (t *turn) runTogether(ctx context.Context, calls []provider.ToolCall, out []outcome) (stop string) {
	ready := make([]*prepared, len(calls))
	for i, c := range calls {
		if stop != "" || ctx.Err() != nil {
			out[i] = outcome{content: "Not run: the turn was stopped.", isErr: true}
			continue
		}
		ready[i], out[i].content, out[i].isErr, stop = t.prepare(ctx, c)
	}
	// They show in the model's order, before any starts.
	for _, p := range ready {
		if p != nil {
			t.host.ToolStart(p.run)
		}
	}
	var wg sync.WaitGroup
	var hostMu sync.Mutex
	for i, p := range ready {
		if p != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[i] = t.exec(ctx, p, &hostMu)
			}()
		}
	}
	wg.Wait()
	return stop
}

// prepared is a call that passed its checks and was approved, ready to run.
type prepared struct {
	call *tools.Call
	run  *ToolRun
}

// prepare checks a call, watches for loops and asks for approval. A call
// that won't run returns nil, with what the model gets back instead.
func (t *turn) prepare(ctx context.Context, c provider.ToolCall) (p *prepared, content string, isErr bool, stop string) {
	s := t.s
	run := &ToolRun{Name: c.Name, Summary: fallbackSummary(c)}
	invalid := func(kind, msg string) (*prepared, string, bool, string) {
		run.Status, run.Error = RunFailed, msg
		t.host.ToolEnd(run)
		if kind == t.failKind {
			t.failures++
		} else {
			t.failKind, t.failures = kind, 1
		}
		if t.failures >= maxFailures {
			return nil, msg, true, fmt.Sprintf("the model made %d invalid tool calls in a row (%s)", t.failures, failureNames[kind])
		}
		return nil, msg, true, ""
	}
	var tool tools.Tool
	if s.tools != nil {
		tool, _ = s.tools.Lookup(c.Name)
	}
	if tool == nil {
		return invalid("unknown", fmt.Sprintf("There is no tool named %q. The tools are: %s.", c.Name, strings.Join(s.toolNames(), ", ")))
	}
	run.Name, run.Kind = tool.Name(), tool.Kind()
	if !s.Available(tool.Kind()) {
		if s.Mode() == config.ApprovalEditor {
			return invalid("blocked", fmt.Sprintf("%s is not available in editor mode, which reads and edits files but runs no commands. "+
				"Finish the edits, then tell the user what to run; the user runs it or switches modes.", tool.Name()))
		}
		return invalid("blocked", fmt.Sprintf("%s is not available in plan mode, which only reads and searches. "+
			"Finish investigating, then present your plan; the user switches modes to carry it out.", tool.Name()))
	}
	args, err := tools.ParseArgs(tool, c.Args)
	if err != nil {
		kind := "params"
		if ae := (*tools.ArgsError)(nil); errors.As(err, &ae) {
			kind = ae.Kind
		}
		return invalid(kind, fmt.Sprintf("Invalid arguments for %s: %v. Its parameters are %s.", tool.Name(), err, paramList(tool)))
	}
	t.failKind, t.failures = "", 0

	// The same call over and over (SPEC §7).
	if key := callKey(tool.Name(), args); key == t.lastCall {
		t.callRepeats++
	} else {
		t.lastCall, t.callRepeats = key, 1
	}
	if t.callRepeats > maxRepeats {
		msg := fmt.Sprintf("Not run: this is call number %d to %s in a row with identical arguments.", t.callRepeats, tool.Name())
		run.Status, run.Error = RunFailed, msg
		t.host.ToolEnd(run)
		return nil, msg, true, fmt.Sprintf("the model called %s with the same arguments %d times in a row", tool.Name(), t.callRepeats)
	}
	if t.callRepeats == maxRepeats {
		t.notes = append(t.notes, fmt.Sprintf("[Note: %s has now been called %d times in a row with identical arguments, so its result won't change. "+
			"Use the result you already have or try a different approach; one more identical call stops this turn.]", tool.Name(), maxRepeats))
	}

	call, err := tool.Prepare(ctx, s.env, args)
	if err != nil {
		run.Status, run.Error = RunFailed, s.redact(err.Error())
		t.host.ToolEnd(run)
		return nil, run.Error, true, ""
	}
	run.Summary, run.Preview, run.Outside = s.redact(call.Summary), s.redact(call.Preview), call.Outside
	run.Question, run.SessionFor = call.Ask, call.SessionFor
	run.Tainted = call.Ask == "" && s.taintAsks(call.Kind)

	if s.needsApproval(call) {
		ans, err := t.host.Approve(ctx, run)
		switch {
		case err != nil:
			run.Status = RunCancelled
			t.host.ToolEnd(run)
			return nil, "Not run: the user didn't answer the approval prompt.", true, "the approval prompt got no answer"
		case !ans.Allow:
			run.Status = RunDenied
			t.host.ToolEnd(run)
			if note := strings.TrimSpace(ans.Note); note != "" {
				return nil, "The user declined this call and said: " + note, true, ""
			}
			return nil, "The user declined this call.", true, "you declined a tool call"
		}
		run.Asked = true
		if ans.Session && call.SessionKey != "" {
			s.allowed[call.SessionKey] = true
		}
	}
	return &prepared{call: call, run: run}, "", false, ""
}

// exec runs a prepared call, which the host was told about, and returns
// what the model gets back. The call's Run finds the host and its run in
// its context (see callFrom). hostMu, when not nil, is held while the
// host is told how the call ended.
func (t *turn) exec(ctx context.Context, p *prepared, hostMu *sync.Mutex) outcome {
	var content string
	var isErr bool
	var images []provider.Image
	s, run := t.s, p.run
	start := time.Now()
	out, err := p.call.Run(withCall(ctx, t.host, run))
	run.Elapsed = time.Since(start)
	switch {
	case ctx.Err() != nil:
		run.Status = RunCancelled
		content = "Cancelled by the user."
		if strings.TrimSpace(out.Output) != "" {
			content += " Output so far:\n" + tools.Cap(s.redact(out.Output))
		}
		isErr = true
	case err != nil:
		run.Status, run.Error = RunFailed, s.redact(err.Error())
		content, isErr = run.Error, true
	default:
		run.Display, run.Warn = out.Display, out.Warn
		content = tools.Cap(s.redact(out.Output))
		images = out.Images
	}
	if out.Tainted {
		s.taint()
	}
	run.Output = content
	if hostMu != nil {
		hostMu.Lock()
		defer hostMu.Unlock()
	}
	t.host.ToolEnd(run)
	return outcome{content, isErr, images}
}

// needsApproval applies the approval modes (SPEC §9). Paths outside the
// workspace are asked about in every mode but yolo (SPEC §3.5), unless
// the user allowed them for the session: all reads outside it at once,
// writes a folder at a time. A call with a question of its own, and
// changes and commands once web content taints the conversation, ask in
// every mode.
func (s *Session) needsApproval(c *tools.Call) bool {
	if c.SessionKey != "" && s.allowed[c.SessionKey] {
		return false
	}
	if c.Ask != "" || s.taintAsks(c.Kind) {
		return true
	}
	mode := s.Mode()
	if len(c.Outside) > 0 && mode != config.ApprovalYolo {
		return true
	}
	switch c.Kind {
	case tools.Edit:
		return mode == config.ApprovalDefault || mode == config.ApprovalPlan
	case tools.Execute:
		return mode != config.ApprovalYolo
	}
	return false
}

func (s *Session) toolNames() []string {
	if s.tools == nil {
		return nil
	}
	var names []string
	for _, name := range s.tools.Names() {
		if tool, _ := s.tools.Lookup(name); s.Available(tool.Kind()) {
			names = append(names, name)
		}
	}
	return names
}

// callKey identifies a call by tool and arguments; encoding/json sorts the
// keys of a map, so equal arguments give equal keys.
func callKey(name string, args tools.Args) string {
	b, _ := json.Marshal(args)
	return name + "\x00" + string(b)
}

// paramList describes a tool's parameters for an error message.
func paramList(tool tools.Tool) string {
	if _, ok := tool.(tools.Schemer); ok {
		return "in its schema"
	}
	var parts []string
	for _, p := range tool.Params() {
		s := p.Name + " (" + p.Type
		if p.Required {
			s += ", required"
		}
		parts = append(parts, s+")")
	}
	return strings.Join(parts, ", ")
}

// fallbackSummary describes a call that failed before its tool could.
func fallbackSummary(c provider.ToolCall) string {
	var args map[string]any
	json.Unmarshal(c.Args, &args)
	for _, k := range []string{"path", "file_path", "command", "pattern"} {
		if v, ok := args[k].(string); ok && v != "" {
			v, _, _ = strings.Cut(strings.TrimSpace(v), "\n")
			if utf8.RuneCountInString(v) > 80 {
				v = string([]rune(v)[:80]) + "…"
			}
			return c.Name + " " + v
		}
	}
	return c.Name
}
