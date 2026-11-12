package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"debbie/internal/effort"
	"debbie/internal/prompt"
	"debbie/internal/provider"
	"debbie/internal/tools"
)

// Subagents (SPEC §10). The task tool hands a bounded piece of work to a
// subagent: a session of its own, with a fresh history, the subagent system
// prompt and the tools its kind allows, never task itself. It works under
// the same approval mode, its approval prompts name it, and its last reply
// is the tool's result, or, in the background, reaches the model by itself.

// maxSubagentAnswer caps, in characters, the reply the parent agent gets.
const maxSubagentAnswer = 8000

// SubagentState is a snapshot of a subagent, for the UI.
type SubagentState struct {
	ID          int // tells subagents apart
	Description string
	Prompt      string    // its task
	Agent       *Subagent // to talk to it
	Full        bool      // it may edit files and run commands, not only read
	Background  bool      // the conversation carries on while it works
	Waiting     bool      // for a free slot: subagents.max_parallel run at once
	Started     time.Time // when it got its slot
	Calls       []SubagentCall
	Done        bool
	Err         string // why it ended without an answer
}

// SubagentCall is one of a subagent's tool calls, in brief.
type SubagentCall struct {
	Summary string
	Display string // its result in brief, once it ended
	Error   string
	Status  RunStatus
	Running bool
}

// SubagentWatcher is a Host that shows subagents as they work. The task
// tool calls it from the subagents' goroutines, possibly several at once.
type SubagentWatcher interface {
	// Subagent shows a subagent's state each time it changes.
	Subagent(task *ToolRun, s SubagentState)
	// SubagentHost returns the host that shows a subagent's own
	// conversation, its replies and calls, as it starts; nil for none.
	// Its Approve isn't used: approvals go to the main host.
	SubagentHost(s SubagentState) Host
}

// BackgroundHost is a Host that can run agents in the background: it stays
// able to ask for approvals between turns, and starts a turn for a report
// that comes while none runs. Any other host runs them while the call waits.
type BackgroundHost interface {
	// BackgroundDone says a background agent's report waits for the model
	// (Session.ReportsMessage). It is called from the agent's goroutine.
	BackgroundDone(description string)
}

// Subagent is a subagent the user can talk to and stop.
type Subagent struct {
	tool *TaskTool
	sub  *Session
	host *subHost

	mu     sync.Mutex
	busy   bool               // a turn is under way, or waiting for a slot
	cancel context.CancelFunc // stops the work under way
}

// Tell passes the user's message to the subagent. One at work gets it
// with its next tool results, or in a turn of its own after; one waiting
// for a slot gets it with its task; one that finished answers it in a new
// turn, which only the user sees.
func (a *Subagent) Tell(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sub.Interject(text)
	if a.busy {
		return
	}
	a.busy = true
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go func() {
		defer cancel()
		if err := a.tool.acquire(ctx); err != nil {
			a.idle()
			return
		}
		defer a.tool.gate.release()
		a.host.restart()
		_, err := a.answer(ctx, "")
		a.host.end(stopReason(ctx, err))
	}()
}

// Stop ends what the subagent is doing.
func (a *Subagent) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

// answer runs turns until no message from the user waits: first task, if
// not empty, then each message that came meanwhile. It returns the last
// turn's result and marks the subagent idle.
func (a *Subagent) answer(ctx context.Context, task string) (Result, error) {
	var res Result
	for {
		a.mu.Lock()
		notes := a.sub.takeInterjections()
		if task == "" && len(notes) == 0 {
			a.busy, a.cancel = false, nil
			a.mu.Unlock()
			return res, nil
		}
		a.mu.Unlock()
		text := task
		for _, n := range notes {
			if text == "" {
				text = n
			} else {
				text += "\n\n" + userNote(n)
			}
		}
		task = ""
		var err error
		if res, err = a.sub.Send(ctx, text, a.host); err != nil {
			a.idle()
			return res, err
		}
	}
}

// idle marks the subagent as doing nothing.
func (a *Subagent) idle() {
	a.mu.Lock()
	a.busy, a.cancel = false, nil
	a.mu.Unlock()
}

// stopReason says why work ended early: "" when it didn't.
func stopReason(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return ""
	case ctx.Err() != nil:
		return "stopped"
	}
	return err.Error()
}

// callContext is what a tool's Run finds in its context: the host of the
// turn and the call's run.
type callContext struct {
	host Host
	run  *ToolRun
}

type callContextKey struct{}

func withCall(ctx context.Context, host Host, run *ToolRun) context.Context {
	return context.WithValue(ctx, callContextKey{}, callContext{host, run})
}

// callFrom returns the host and run of the call whose context ctx is. A
// call run outside a turn gets a host that refuses every approval.
func callFrom(ctx context.Context) (Host, *ToolRun) {
	if c, ok := ctx.Value(callContextKey{}).(callContext); ok {
		return c.host, c.run
	}
	return refusingHost{}, &ToolRun{Name: "task"}
}

// refusingHost shows nothing and allows nothing.
type refusingHost struct{}

func (refusingHost) ReplyStart()          {}
func (refusingHost) Event(provider.Event) {}
func (refusingHost) ReplyEnd()            {}
func (refusingHost) ToolStart(*ToolRun)   {}
func (refusingHost) ToolEnd(*ToolRun)     {}
func (refusingHost) Approve(context.Context, *ToolRun) (Approval, error) {
	return Approval{}, errors.New("no one to ask")
}

// Workers is what agents run on at the moment, which the user may change
// while the session runs (/provider workers).
type Workers struct {
	Provider  provider.Provider // nil: the conversation's
	MaxTokens int               // the output limit of Provider
	// Effort is how hard they reason; effort.Default is the conversation's
	// at most medium: they do bounded work, and their reports are checked.
	Effort   effort.Level
	Parallel int  // how many run at once
	Vision   bool // Provider's model sees images; with no Provider, the conversation's rule applies
	// CompactAt and ContextWindow are when an agent on Provider compacts its
	// conversation, and the size of its context; with no Provider, the
	// conversation's apply.
	CompactAt, ContextWindow int
}

// workerEffort is how hard the conversation's reasoning level has agents
// reason, unless the workers set their own.
const workerEffort = effort.Medium

// TaskTool is the task tool of a session.
type TaskTool struct {
	parent   *Session
	system   func(full bool) string
	workers  func() (Workers, error)
	maxTurns int
	gate     slotGate
	approve  sync.Mutex // one subagent's approval prompt at a time
	ids      atomic.Int64
}

// NewTaskTool returns the task tool of parent. system writes a subagent's
// system prompt; workers says what they run on, read as each one starts;
// each runs for at most maxTurns requests.
func NewTaskTool(parent *Session, system func(full bool) string, workers func() (Workers, error), maxTurns int) *TaskTool {
	return &TaskTool{parent: parent, system: system, workers: workers, maxTurns: max(1, maxTurns)}
}

// acquire waits for a free slot.
func (t *TaskTool) acquire(ctx context.Context) error {
	w, err := t.workers()
	if err != nil {
		return err
	}
	return t.gate.acquire(ctx, max(1, w.Parallel))
}

func (t *TaskTool) Name() string        { return "task" }
func (t *TaskTool) Kind() tools.Kind    { return tools.Read } // its subagent asks for each call that needs it
func (t *TaskTool) Description() string { return prompt.Tool("task") }

func (t *TaskTool) Params() []tools.Param {
	return []tools.Param{
		{Name: "description", Type: "string", Required: true, Description: "A short (3-5 word) description of the task, shown to the user"},
		{Name: "prompt", Type: "string", Required: true, Description: "The task for the agent, with everything it needs to know: it doesn't see this conversation"},
		{Name: "tools", Type: "string", Description: `"read-only" (the default): it can read and search files; "full": it can also edit files and run commands`},
		{Name: "background", Type: "boolean", Description: "true returns at once and you carry on; the report reaches you by itself when the agent finishes"},
	}
}

func (t *TaskTool) Prepare(_ context.Context, _ *tools.Env, args tools.Args) (*tools.Call, error) {
	desc := strings.Join(strings.Fields(args.String("description")), " ")
	task := strings.TrimSpace(args.String("prompt"))
	kind := args.String("tools")
	switch {
	case desc == "":
		return nil, errors.New("description is empty: give the task a few words that the user sees")
	case task == "":
		return nil, errors.New("prompt is empty: give the agent its task in full")
	case kind == "":
		kind = "read-only"
	case kind != "read-only" && kind != "full":
		return nil, fmt.Errorf(`tools is %q: use "read-only" or "full"`, kind)
	}
	if utf8.RuneCountInString(desc) > 60 {
		desc = string([]rune(desc)[:60]) + "…"
	}
	full, background := kind == "full", args.Bool("background")
	summary := "task " + desc + " · " + kind
	if background {
		summary += " · background"
	}
	return &tools.Call{Kind: tools.Read, Summary: summary,
		Run: func(ctx context.Context) (tools.Result, error) { return t.run(ctx, desc, task, full, background) }}, nil
}

// run starts a subagent, once a slot is free, and returns its answer; in
// the background it returns at once, and the answer reaches the model by
// itself.
func (t *TaskTool) run(ctx context.Context, desc, task string, full, background bool) (tools.Result, error) {
	host, run := callFrom(ctx)
	sub, err := t.session(full)
	if err != nil {
		return tools.Result{}, err
	}
	bg, canBG := host.(BackgroundHost)
	background = background && canBG
	id := int(t.ids.Add(1))
	h := &subHost{parent: host, task: run, desc: desc, id: id, approve: &t.approve}
	a := &Subagent{tool: t, sub: sub, host: h, busy: true}
	h.state = SubagentState{ID: id, Description: desc, Prompt: task, Agent: a, Full: full, Background: background, Waiting: true}
	if w, ok := host.(SubagentWatcher); ok {
		h.watch = w
		h.show = w.SubagentHost(h.state)
	}
	h.update()
	if background {
		// Its context isn't the turn's: the turn may end long before it does.
		bctx, cancel := context.WithCancel(context.Background())
		a.mu.Lock()
		a.cancel = cancel
		a.mu.Unlock()
		gen := t.parent.generation()
		go func() {
			defer cancel()
			out, err := t.work(bctx, a, h, task)
			report := fmt.Sprintf("[Background agent %d, %q, finished. Its report:\n\n%s]", id, desc, out.Output)
			switch {
			case bctx.Err() != nil:
				report = fmt.Sprintf("[The user stopped background agent %d, %q, before it finished.]", id, desc)
			case err != nil:
				report = fmt.Sprintf("[Background agent %d, %q, failed: %s]", id, desc, t.parent.redact(err.Error()))
			}
			if out.Tainted {
				t.parent.taint()
			}
			if t.parent.report(gen, report) {
				bg.BackgroundDone(desc)
			}
		}()
		return tools.Result{
			Output:  fmt.Sprintf("Agent %d started in the background. Its report reaches you by itself when it finishes, so don't wait or poll for it: carry on with other work, or end your turn.", id),
			Display: "in the background",
		}, nil
	}
	// Its own context, so the user can stop it alone.
	turn := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	out, err := t.work(ctx, a, h, task)
	if err != nil {
		if ctx.Err() != nil && turn.Err() == nil {
			// The user stopped this subagent, not the turn.
			return tools.Result{Output: "The user stopped this agent before it finished."}, nil
		}
		return tools.Result{}, err
	}
	return out, nil
}

// work waits for a free slot and runs the subagent's task, ending its state.
func (t *TaskTool) work(ctx context.Context, a *Subagent, h *subHost, task string) (tools.Result, error) {
	if err := t.acquire(ctx); err != nil {
		a.idle()
		if ctx.Err() != nil {
			h.end("stopped before it started")
			return tools.Result{}, ctx.Err()
		}
		h.end(err.Error())
		return tools.Result{}, err
	}
	defer t.gate.release()
	h.restart()

	res, err := a.answer(ctx, task)
	if err != nil {
		h.end(stopReason(ctx, err))
		return tools.Result{}, err
	}
	answer := strings.TrimSpace(res.Text)
	if answer == "" {
		answer = "(The agent ended without an answer.)"
	}
	if res.Stopped != "" {
		answer += "\n\n[The agent stopped early: " + res.Stopped + ". This is what it had found.]"
	}
	if utf8.RuneCountInString(answer) > maxSubagentAnswer {
		answer = string([]rune(answer)[:maxSubagentAnswer]) + "\n\n[The answer was cut at 8,000 characters.]"
	}
	h.end("")
	h.mu.Lock()
	calls := len(h.state.Calls)
	h.mu.Unlock()
	return tools.Result{Output: answer, Display: tools.Count(calls, "call", "calls"), Tainted: a.sub.Tainted()}, nil
}

// session makes the subagent's session: the provider, output limit and
// effort the workers have, or else the parent's provider and limit and its
// effort at most at workerEffort; the parent's mode, reasoning budget and
// session approvals; and the tools its kind allows.
func (t *TaskTool) session(full bool) (*Session, error) {
	p := t.parent
	p.mu.Lock()
	prov, maxTokens, mode, level, budget, web, tainted := p.provider, p.maxTokens, p.mode, p.effort, p.budget, p.web, p.tainted
	compactAt, compactWindow := p.compactAt, p.compactWindow
	p.mu.Unlock()
	w, err := t.workers()
	if err != nil {
		return nil, fmt.Errorf("the agents' provider: %w", err)
	}
	if w.Provider != nil {
		prov, maxTokens, compactAt, compactWindow = w.Provider, w.MaxTokens, w.CompactAt, w.ContextWindow
	}
	if w.Effort != effort.Default {
		level = w.Effort
	} else {
		level = effort.AtMost(level, workerEffort)
	}
	sub := New(prov, t.system(full), maxTokens)
	sub.mode, sub.effort, sub.budget, sub.redact, sub.maxTurns = mode, level, budget, p.redact, t.maxTurns
	sub.noStall = true // it has maxTurns, and reading may be its whole task
	sub.compactAt, sub.compactWindow = compactAt, compactWindow
	sub.vision = p.vision
	if w.Provider != nil {
		sees := w.Vision
		sub.vision = func() bool { return sees }
	}
	sub.ledger = p.ledger // its spend is the session's
	// Its task may carry what a page said: the parent's taint goes along.
	sub.web, sub.tainted = web, tainted
	sub.allowed = maps.Clone(p.allowed) // read while the parent waits for this call
	sub.SetTools(subagentTools(p.tools, full), p.env)
	return sub, nil
}

// subagentTools are a subagent's tools: those that only read, or with full
// all of them; never task, so a subagent starts no subagents, nor ask_user,
// since only the user's own agent asks them anything, and never
// the browser's, whose one tab subagents working side by side would share.
func subagentTools(reg *tools.Registry, full bool) *tools.Registry {
	sub := tools.NewRegistry()
	if reg == nil {
		return sub
	}
	for _, name := range reg.Names() {
		if tool, ok := reg.Lookup(name); ok && name != "task" && name != "ask_user" && !strings.HasPrefix(name, "browser_") && (full || tool.Kind() == tools.Read) {
			sub.Add(tool)
		}
	}
	return sub
}

// slotGate limits how many subagents work at once. The limit comes with
// each acquire, since the provider they run on can change: one that waits
// goes first when a slot frees below its limit.
type slotGate struct {
	mu      sync.Mutex
	running int
	waiting []*slotWaiter
}

type slotWaiter struct {
	limit int
	ready chan struct{}
}

func (g *slotGate) acquire(ctx context.Context, limit int) error {
	g.mu.Lock()
	if g.running < limit && len(g.waiting) == 0 {
		g.running++
		g.mu.Unlock()
		return nil
	}
	w := &slotWaiter{limit: limit, ready: make(chan struct{})}
	g.waiting = append(g.waiting, w)
	g.mu.Unlock()
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		defer g.mu.Unlock()
		select {
		case <-w.ready: // it got the slot just then
			g.running--
			g.wake()
		default:
			g.waiting = slices.DeleteFunc(g.waiting, func(o *slotWaiter) bool { return o == w })
		}
		return ctx.Err()
	}
}

func (g *slotGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running--
	g.wake()
}

// wake gives free slots to those waiting, in order.
func (g *slotGate) wake() {
	for len(g.waiting) > 0 && g.running < g.waiting[0].limit {
		close(g.waiting[0].ready)
		g.waiting = g.waiting[1:]
		g.running++
	}
}

// subHost is the host of a subagent's session. Its replies aren't shown;
// its calls are, under the task call, through the SubagentWatcher; and its
// approval prompts go to the parent's host, naming it.
type subHost struct {
	parent  Host
	watch   SubagentWatcher
	show    Host // the UI's view of its conversation; nil for none
	task    *ToolRun
	desc    string
	id      int
	approve *sync.Mutex

	mu    sync.Mutex // guards state and runs
	state SubagentState
	runs  map[*ToolRun]int // each call's place in state.Calls
}

func (h *subHost) ReplyStart() {
	if h.show != nil {
		h.show.ReplyStart()
	}
}

func (h *subHost) Event(e provider.Event) {
	if h.show != nil {
		h.show.Event(e)
	}
}

func (h *subHost) ReplyEnd() {
	if h.show != nil {
		h.show.ReplyEnd()
	}
}

func (h *subHost) Approve(ctx context.Context, run *ToolRun) (Approval, error) {
	run.Agent, run.AgentID = h.desc, h.id
	h.approve.Lock()
	defer h.approve.Unlock()
	return h.parent.Approve(ctx, run)
}

func (h *subHost) ToolStart(run *ToolRun) {
	h.call(run, true)
	if h.show != nil {
		h.show.ToolStart(run)
	}
}

func (h *subHost) ToolEnd(run *ToolRun) {
	h.call(run, false)
	if h.show != nil {
		h.show.ToolEnd(run)
	}
}

// restart records that the subagent got a slot and works.
func (h *subHost) restart() {
	h.mu.Lock()
	h.state.Waiting, h.state.Done, h.state.Err, h.state.Started = false, false, "", time.Now()
	h.mu.Unlock()
	h.update()
}

func (h *subHost) call(run *ToolRun, running bool) {
	h.mu.Lock()
	if h.runs == nil {
		h.runs = map[*ToolRun]int{}
	}
	i, ok := h.runs[run]
	if !ok {
		i = len(h.state.Calls)
		h.runs[run] = i
		h.state.Calls = append(h.state.Calls, SubagentCall{})
	}
	h.state.Calls[i] = SubagentCall{Summary: run.Summary, Display: run.Display, Error: run.Error, Status: run.Status, Running: running}
	h.mu.Unlock()
	h.update()
}

// end records how the subagent ended and shows it.
func (h *subHost) end(err string) {
	h.mu.Lock()
	h.state.Done, h.state.Waiting, h.state.Err = true, false, err
	h.mu.Unlock()
	h.update()
}

// update shows the subagent as it is now.
func (h *subHost) update() {
	if h.watch == nil {
		return
	}
	h.mu.Lock()
	s := h.state
	s.Calls = slices.Clone(s.Calls)
	h.mu.Unlock()
	h.watch.Subagent(h.task, s)
}
