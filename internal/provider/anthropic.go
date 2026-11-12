package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"debbie/internal/app"
	"debbie/internal/config"
	"debbie/internal/effort"
	"debbie/internal/netguard"
)

// AnthropicVersion is the version of the Claude API the adapter speaks
// (SPEC §4.4). It takes no sampling parameters, which the agent doesn't
// send to Claude.
const AnthropicVersion = "2026-10-21"

// thinkingControlsBeta lets a request ask the API to drop the thinking
// blocks that a changed history invalidates, instead of failing with 400:
// switching the approval mode changes the system prompt and the tools,
// which the newest models check their earlier thinking against.
const thinkingControlsBeta = "thinking-binding-controls-2026-08-01"

// thinkingUpdatesBeta lets a request ask for display "updates": on the
// newest models the short notes written between tool calls come back as
// thinking blocks, and this returns their text while reasoning stays hidden.
const thinkingUpdatesBeta = "thinking-display-updates-2026-08-18"

// interruptedNote is the text of a progress block that stands in for work
// cut off by the output limit; it is no note to show.
const interruptedNote = "This part of the response was interrupted before it finished."

// defaultAnthropicMaxTokens is max_tokens when neither the request nor the
// provider sets one; the API requires it.
const defaultAnthropicMaxTokens = 32000

// Anthropic talks to the Claude API's Messages endpoint (SPEC §4.4).
type Anthropic struct {
	name       string
	client     *http.Client
	baseURL    string // without a trailing slash
	model      string
	apiKey     string
	keyEnv     string // the variable the key came from, for NoKeyError
	official   bool   // baseURL is Claude's own API, which needs a key
	workspace  string // the workspace for a key that isn't scoped to one
	maxTokens  int    // when a request leaves it 0
	extraBody  map[string]json.RawMessage
	streamIdle time.Duration
	redactor   *config.Redactor

	mu   sync.Mutex
	caps *ModelInfo // what the model supports, once known
}

// NewAnthropic returns an adapter for provider p, called name in messages.
func NewAnthropic(name string, p *config.Provider, apiKey string) (*Anthropic, error) {
	ep, err := netguard.EndpointFromURL(p.BaseURL, p.TLSPinSHA256)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", name, err)
	}
	client, err := netguard.NewClient(ep, netguard.Options{HeaderTimeout: p.HeaderTimeout(), ProviderName: name})
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", name, err)
	}
	u, _ := url.Parse(p.BaseURL)
	return &Anthropic{
		name:       name,
		client:     client,
		baseURL:    strings.TrimRight(p.BaseURL, "/"),
		model:      p.Model,
		apiKey:     apiKey,
		keyEnv:     p.APIKeyEnv,
		official:   u != nil && strings.EqualFold(u.Hostname(), "api.anthropic.com"),
		workspace:  p.Options.WorkspaceID,
		maxTokens:  p.MaxOutputTokens,
		extraBody:  p.Options.ExtraBody,
		streamIdle: p.StreamIdle(),
		redactor:   config.NewRedactor(apiKey),
	}, nil
}

// Model returns the model that requests ask for.
func (a *Anthropic) Model() string { return a.model }

// SetModel changes the model that requests ask for.
func (a *Anthropic) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model, a.caps = model, nil
}

// SetCapabilities records what the model supports, as Describe or Models
// reported it.
func (a *Anthropic) SetCapabilities(m ModelInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.caps = &m
}

// EffortLevels are the effort levels the model takes, weakest first: as
// the models API said, or, when it couldn't be asked, the levels every
// model with adaptive thinking takes.
func (a *Anthropic) EffortLevels() []effort.Level {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.caps != nil:
		return a.caps.EffortLevels
	case updatesByName(a.model): // the newest models take xhigh as well
		return []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max}
	case adaptiveByName(a.model):
		return []effort.Level{effort.Low, effort.Medium, effort.High, effort.Max}
	}
	return nil
}

// CachesPrefix says side requests should repeat the conversation's prefix.
func (a *Anthropic) CachesPrefix() bool { return true }

// updates reports whether the model returns its notes between tool calls as
// thinking blocks, which requests then ask for by themselves (display
// "updates").
func (a *Anthropic) updates() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return updatesByName(a.model)
}

// adaptive reports whether requests turn adaptive thinking on.
func (a *Anthropic) adaptive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.caps != nil {
		return a.caps.Adaptive
	}
	return adaptiveByName(a.model)
}

var claudeModelRE = regexp.MustCompile(`^claude-(opus|sonnet|haiku|fable|mythos)-(\d+)(?:-(\d{1,2}))?(?:-|$)`)

// adaptiveByName reports whether a model has adaptive thinking, by its
// ID, for when the models API can't be asked: Opus and Sonnet from 4.6,
// Haiku from 5, and every Fable and Mythos.
func adaptiveByName(model string) bool {
	if strings.HasPrefix(model, "claude-fable-") || strings.HasPrefix(model, "claude-mythos-") {
		return true
	}
	m := claudeModelRE.FindStringSubmatch(model)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[2])
	minor, _ := strconv.Atoi(m[3])
	if m[1] == "haiku" {
		return major >= 5
	}
	return major > 4 || major == 4 && minor >= 6
}

// updatesByName reports whether a model takes display "updates": Opus and
// Sonnet from 5.5, Fable, and Mythos from 5.1.
func updatesByName(model string) bool {
	m := claudeModelRE.FindStringSubmatch(model)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[2])
	minor, _ := strconv.Atoi(m[3])
	switch m[1] {
	case "fable":
		return true
	case "mythos":
		return major > 5 || major == 5 && minor >= 1
	case "opus", "sonnet":
		return major > 5 || major == 5 && minor >= 5
	}
	return false
}

type anthropicRequest struct {
	Model        string             `json:"model"`
	MaxTokens    int                `json:"max_tokens"`
	System       []anthropicText    `json:"system,omitempty"`
	Messages     []anthropicMessage `json:"messages"`
	Tools        []anthropicTool    `json:"tools,omitempty"`
	Thinking     *anthropicThinking `json:"thinking,omitempty"`
	OutputConfig *outputConfig      `json:"output_config,omitempty"`
	// CacheControl caches the conversation up to its last block; the system
	// prompt has a breakpoint of its own.
	CacheControl *cacheControl `json:"cache_control,omitempty"`
	Stream       bool          `json:"stream"`
}

type anthropicText struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

var ephemeral = &cacheControl{Type: "ephemeral"}

type anthropicThinking struct {
	Type         string        `json:"type"`
	Display      string        `json:"display,omitempty"`
	BlockBinding *blockBinding `json:"block_binding,omitempty"`
}

type blockBinding struct {
	PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
}

type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type anthropicMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	// EagerInputStreaming streams a tool's input as it is written, so a
	// long file shows while it comes; the agent checks the JSON once the
	// call is complete.
	EagerInputStreaming bool `json:"eager_input_streaming,omitempty"`
}

func (a *Anthropic) requestBody(req Request) ([]byte, bool, error) {
	a.mu.Lock()
	model, caps := a.model, a.caps
	a.mu.Unlock()
	body := anthropicRequest{
		Model:        model,
		MaxTokens:    req.MaxTokens,
		Messages:     anthropicMessages(req.Messages),
		CacheControl: ephemeral,
		Stream:       true,
	}
	if body.MaxTokens <= 0 {
		body.MaxTokens = a.maxTokens
	}
	if body.MaxTokens <= 0 {
		body.MaxTokens = defaultAnthropicMaxTokens
	}
	if caps != nil && caps.MaxOutput > 0 {
		body.MaxTokens = min(body.MaxTokens, caps.MaxOutput)
	}
	if req.System != "" {
		body.System = []anthropicText{{Type: "text", Text: req.System, CacheControl: ephemeral}}
	}
	for _, t := range req.Tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: schema, EagerInputStreaming: true})
	}
	adaptive := a.adaptive()
	if adaptive {
		display := "summarized"
		if a.updates() {
			display = "updates"
		}
		body.Thinking = &anthropicThinking{Type: "adaptive", Display: display,
			BlockBinding: &blockBinding{PrefixMismatchBehavior: "drop_block"}}
	}
	if levels := a.EffortLevels(); len(levels) > 0 && req.Effort != effort.Default {
		l := effort.Fit(req.Effort, levels)
		if l == effort.None { // the newest models can't stop thinking: the least there is
			l = levels[0]
		}
		body.OutputConfig = &outputConfig{Effort: string(l)}
	}
	data, err := json.Marshal(body)
	if err != nil || len(a.extraBody) == 0 {
		return data, adaptive, err
	}
	// extra_body adds fields; it never replaces one set above.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, false, err
	}
	for k, v := range a.extraBody {
		if _, set := fields[k]; !set {
			fields[k] = v
		}
	}
	data, err = json.Marshal(fields)
	return data, adaptive, err
}

// anthropicMessages turns the conversation into Messages API messages:
// alternating roles, each assistant turn's blocks in the order the model
// wrote them, thinking blocks verbatim, and every tool call answered by a
// result first thing in the next user message. The same conversation
// always gives the same bytes, which keeps the prompt cache and the
// model's earlier thinking valid. Every image stays: replacing an old one
// would be an edit of the history.
func anthropicMessages(msgs []Message) []anthropicMessage {
	var out []anthropicMessage
	add := func(role string, blocks []json.RawMessage) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropicMessage{Role: role, Content: blocks})
	}
	for _, m := range msgs {
		var blocks []json.RawMessage
		if m.Role == RoleAssistant {
			for _, p := range m.Parts {
				switch {
				case p.Kind == PartReasoning && len(p.Opaque) > 0:
					blocks = append(blocks, p.Opaque)
				case p.Kind == PartText && strings.TrimSpace(p.Text) != "":
					blocks = append(blocks, textBlock(p.Text))
				case p.Kind == PartToolCall && p.ToolCall != nil:
					blocks = append(blocks, toolUseBlock(p.ToolCall))
				}
			}
			add("assistant", blocks)
			continue
		}
		var text []json.RawMessage
		for _, p := range m.Parts {
			switch {
			case p.Kind == PartToolResult && p.ToolResult != nil:
				blocks = append(blocks, toolResultBlock(p.ToolResult))
			case p.Kind == PartText && strings.TrimSpace(p.Text) != "":
				text = append(text, textBlock(p.Text))
			}
		}
		add("user", append(blocks, text...))
	}
	out = answerToolCalls(out)
	if len(out) > 0 && out[0].Role == "assistant" { // the conversation must start with the user
		out = append([]anthropicMessage{{Role: "user", Content: []json.RawMessage{textBlock("(The conversation so far.)")}}}, out...)
	}
	return out
}

// answerToolCalls makes each user message after tool calls start with
// their results, one for each call: a call without one, such as one
// interrupted, gets a result saying it didn't run, and a result for no
// call of the turn before is left out.
func answerToolCalls(msgs []anthropicMessage) []anthropicMessage {
	for i := 0; i < len(msgs); i++ {
		if msgs[i].Role != "assistant" {
			continue
		}
		var ids []string
		for _, b := range msgs[i].Content {
			var blk struct{ Type, ID string }
			if json.Unmarshal(b, &blk) == nil && blk.Type == "tool_use" {
				ids = append(ids, blk.ID)
			}
		}
		if i+1 == len(msgs) || msgs[i+1].Role != "user" {
			if len(ids) == 0 {
				continue
			}
			msgs = append(msgs[:i+1], append([]anthropicMessage{{Role: "user"}}, msgs[i+1:]...)...)
		}
		next := &msgs[i+1]
		results := map[string]json.RawMessage{}
		var rest []json.RawMessage
		for _, b := range next.Content {
			var blk struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			}
			if json.Unmarshal(b, &blk) == nil && blk.Type == "tool_result" {
				if _, dup := results[blk.ToolUseID]; !dup {
					results[blk.ToolUseID] = b
				}
				continue
			}
			rest = append(rest, b)
		}
		var content []json.RawMessage
		for _, id := range ids {
			r, ok := results[id]
			if !ok {
				r = toolResultBlock(&ToolResult{CallID: id, Content: "The call didn't run.", IsError: true})
			}
			content = append(content, r)
		}
		next.Content = append(content, rest...)
		if len(next.Content) == 0 {
			msgs = append(msgs[:i+1], msgs[i+2:]...)
		}
	}
	return msgs
}

func textBlock(text string) json.RawMessage {
	b, _ := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{"text", text})
	return b
}

// toolUseBlock sends back a call. Arguments that aren't a JSON object,
// which the model can write while streaming, go back as an empty object:
// the call's result told the model what was wrong.
func toolUseBlock(c *ToolCall) json.RawMessage {
	input := json.RawMessage(bytes.TrimSpace(c.Args))
	if !json.Valid(input) || len(input) == 0 || input[0] != '{' {
		input = json.RawMessage(`{}`)
	}
	b, _ := json.Marshal(struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}{"tool_use", c.ID, c.Name, input})
	return b
}

func toolResultBlock(r *ToolResult) json.RawMessage {
	text := r.Content
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	type source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	type block struct {
		Type   string  `json:"type"`
		Text   string  `json:"text,omitempty"`
		Source *source `json:"source,omitempty"`
	}
	var content any = text
	if len(r.Images) > 0 {
		blocks := []block{{Type: "text", Text: text}}
		for _, img := range r.Images {
			blocks = append(blocks, block{Type: "image", Source: &source{Type: "base64", MediaType: img.MediaType,
				Data: base64.StdEncoding.EncodeToString(img.Data)}})
		}
		content = blocks
	}
	b, _ := json.Marshal(struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		Content   any    `json:"content"`
		IsError   bool   `json:"is_error,omitempty"`
	}{"tool_result", r.CallID, content, r.IsError})
	return b
}

// NoKeyError is a request to Claude without an API key: the variable the
// provider reads its key from isn't set, or it has none.
type NoKeyError struct {
	Provider string // the provider's name
	EnvVar   string // the variable it reads the key from; "" when the key belongs in the config
}

func (e *NoKeyError) Error() string {
	if e.EnvVar != "" {
		return fmt.Sprintf("%s has no API key: %s isn't set where %s started", e.Provider, e.EnvVar, app.Name)
	}
	return e.Provider + " has no API key"
}

// noKey stops a request that has no key from going to Claude's own API,
// which refuses it; a proxy may add the key itself.
func (a *Anthropic) noKey() error {
	if a.apiKey == "" && a.official {
		return &NoKeyError{Provider: a.name, EnvVar: a.keyEnv}
	}
	return nil
}

// Stream sends req and streams the reply.
func (a *Anthropic) Stream(ctx context.Context, req Request) (Stream, error) {
	if err := a.noKey(); err != nil {
		return nil, err
	}
	body, thinking, err := a.requestBody(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		cancel(nil)
		return nil, err
	}
	a.setHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if thinking {
		beta := thinkingControlsBeta
		if a.updates() {
			beta += "," + thinkingUpdatesBeta
		}
		httpReq.Header.Set("anthropic-beta", beta)
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		err = a.transportError(ctx, err)
		cancel(nil)
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := a.apiError(resp)
		resp.Body.Close()
		cancel(nil)
		return nil, err
	}
	s := &anthropicStream{ctx: ctx, cancel: cancel, body: resp.Body, provider: a.name, redactor: a.redactor,
		blocks: map[int]*anthropicBlock{}, notes: thinking && a.updates()}
	if isJSON(resp.Header.Get("Content-Type")) {
		s.readWhole() // a proxy that ignored "stream": true
		return s, nil
	}
	s.idle = newIdleReader(resp.Body, a.streamIdle, func() {
		cancel(&IdleError{Provider: a.name, After: a.streamIdle})
	})
	s.sse = newSSEReader(s.idle)
	return s, nil
}

func (a *Anthropic) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", app.Name+"/"+app.Version)
	req.Header.Set("anthropic-version", AnthropicVersion)
	if a.apiKey != "" {
		req.Header.Set("x-api-key", a.apiKey)
	}
	if a.workspace != "" {
		req.Header.Set("anthropic-workspace-id", a.workspace)
	}
}

func (a *Anthropic) transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return a.redactor.Error(fmt.Errorf("%s: %w", a.name, err))
}

func (a *Anthropic) apiError(resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized && a.apiKey == "" {
		return &NoKeyError{Provider: a.name, EnvVar: a.keyEnv} // a proxy that wants one
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	typ, msg := anthropicErrorText(data)
	return &APIError{
		Provider:   a.name,
		Status:     resp.StatusCode,
		Type:       typ,
		Message:    a.redactor.String(msg),
		RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
}

// anthropicErrorText reads an error body: {"type":"error","error":{"type",
// "message"},"request_id"}, the request's ID kept for a support request.
func anthropicErrorText(data []byte) (typ, msg string) {
	var body struct {
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(data, &body) != nil || body.Error == nil || body.Error.Message == "" {
		return "", errorMessage(data)
	}
	msg = body.Error.Message
	if body.RequestID != "" {
		msg += " (request " + body.RequestID + ")"
	}
	return body.Error.Type, msg
}

// Models lists the models the API offers, newest first, with what each
// supports. It also tells whether the key works.
func (a *Anthropic) Models(ctx context.Context) ([]ModelInfo, error) {
	var models []ModelInfo
	after := ""
	for page := 0; page < 20; page++ {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		var body struct {
			Data    []anthropicModel `json:"data"`
			HasMore bool             `json:"has_more"`
			LastID  string           `json:"last_id"`
		}
		if err := a.get(ctx, "/v1/models?"+q.Encode(), &body); err != nil {
			return nil, err
		}
		for _, m := range body.Data {
			models = append(models, m.info())
		}
		if !body.HasMore || body.LastID == "" {
			break
		}
		after = body.LastID
	}
	return models, nil
}

// Workspace is a workspace of the Claude Console.
type Workspace struct {
	ID, Name string
}

// Workspaces lists the organization's workspaces, the default one first,
// for a key that isn't scoped to one; a key scoped to a workspace may not
// read the list.
func (a *Anthropic) Workspaces(ctx context.Context) ([]Workspace, error) {
	var body struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := a.get(ctx, "/v1/organizations/workspaces?include_default=true&limit=1000", &body); err != nil {
		return nil, err
	}
	var out []Workspace
	for _, w := range body.Data {
		out = append(out, Workspace{ID: w.ID, Name: w.Name})
	}
	return out, nil
}

// Describe returns what one model supports.
func (a *Anthropic) Describe(ctx context.Context, id string) (ModelInfo, error) {
	var m anthropicModel
	if err := a.get(ctx, "/v1/models/"+url.PathEscape(id), &m); err != nil {
		return ModelInfo{}, err
	}
	return m.info(), nil
}

func (a *Anthropic) get(ctx context.Context, path string, into any) error {
	if err := a.noKey(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+path, nil)
	if err != nil {
		return err
	}
	a.setHeaders(req)
	resp, err := a.client.Do(req)
	if err != nil {
		return a.transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.apiError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxEventSize)).Decode(into); err != nil {
		return fmt.Errorf("%s sent a malformed reply to %s: %v", a.name, strings.SplitN(path, "?", 2)[0], err)
	}
	return nil
}

// anthropicModel is a model as the models API describes it.
type anthropicModel struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	MaxInput     int    `json:"max_input_tokens"`
	MaxTokens    int    `json:"max_tokens"`
	Capabilities struct {
		ImageInput supported `json:"image_input"`
		Thinking   struct {
			Types struct {
				Adaptive supported `json:"adaptive"`
			} `json:"types"`
		} `json:"thinking"`
		Effort map[string]json.RawMessage `json:"effort"`
	} `json:"capabilities"`
}

type supported struct {
	Supported bool `json:"supported"`
}

func (m anthropicModel) info() ModelInfo {
	info := ModelInfo{ID: m.ID, Name: m.DisplayName, ContextWindow: m.MaxInput, MaxOutput: m.MaxTokens,
		Vision: m.Capabilities.ImageInput.Supported, Adaptive: m.Capabilities.Thinking.Types.Adaptive.Supported}
	var on bool
	if json.Unmarshal(m.Capabilities.Effort["supported"], &on) == nil && on {
		for _, l := range []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max} {
			var s supported
			if json.Unmarshal(m.Capabilities.Effort[string(l)], &s) == nil && s.Supported {
				info.EffortLevels = append(info.EffortLevels, l)
			}
		}
	}
	return info
}

// anthropicStream reads a streamed reply: its content blocks in order,
// each started, filled by deltas and stopped (SPEC §4.4).
type anthropicStream struct {
	ctx      context.Context
	cancel   context.CancelCauseFunc
	body     io.ReadCloser
	sse      *sseReader
	idle     *idleReader
	provider string
	redactor *config.Redactor

	queue  []Event
	blocks map[int]*anthropicBlock // the open blocks, by index
	usage  anthropicUsage
	stop   string
	detail string
	ended  bool
	err    error
	closed bool
	notes  bool // the request asked for display "updates": thinking text is a note
}

// anthropicBlock is a content block being streamed.
type anthropicBlock struct {
	kind  string
	start json.RawMessage // the block as it started
	id    string          // a tool_use block's ID
	text  strings.Builder // a thinking block's thinking
	sig   strings.Builder // and its signature
	input bool            // a tool_use block got some input
}

// anthropicUsage counts tokens; the input is the whole prompt, the part
// read from or written to the cache included.
type anthropicUsage struct {
	Input, CacheWrite, CacheRead, Output int
	seen                                 bool
}

func (u *anthropicUsage) add(d *usageFields) {
	if d == nil {
		return
	}
	for _, f := range []struct {
		v  *int
		to *int
	}{{d.Input, &u.Input}, {d.CacheWrite, &u.CacheWrite}, {d.CacheRead, &u.CacheRead}, {d.Output, &u.Output}} {
		if f.v != nil {
			*f.to, u.seen = *f.v, true // counts are cumulative
		}
	}
}

type usageFields struct {
	Input      *int `json:"input_tokens"`
	CacheWrite *int `json:"cache_creation_input_tokens"`
	CacheRead  *int `json:"cache_read_input_tokens"`
	Output     *int `json:"output_tokens"`
}

type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage *usageFields `json:"usage"`
	} `json:"message"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
		StopDetails *struct {
			Category    string `json:"category"`
			Explanation string `json:"explanation"`
		} `json:"stop_details"`
	} `json:"delta"`
	Usage *usageFields `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *anthropicStream) Recv() (Event, error) {
	for len(s.queue) == 0 {
		if s.err != nil {
			return nil, s.err
		}
		if s.ended {
			s.err = io.EOF
			s.Close()
			continue
		}
		ev, err := s.sse.Next()
		if err == nil {
			err = s.handle(ev)
		}
		if err != nil {
			s.fail(err)
		}
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	return ev, nil
}

func (s *anthropicStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.idle != nil {
		s.idle.stop()
	}
	s.cancel(nil)
	return s.body.Close()
}

func (s *anthropicStream) handle(raw sseEvent) error {
	var ev anthropicEvent
	if err := json.Unmarshal([]byte(raw.Data), &ev); err != nil {
		return fmt.Errorf("malformed event: %v", err)
	}
	if ev.Type == "" {
		ev.Type = raw.Type
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			s.usage.add(ev.Message.Usage)
		}
	case "content_block_start":
		s.startBlock(ev.Index, ev.ContentBlock)
	case "content_block_delta":
		if b := s.blocks[ev.Index]; b != nil && ev.Delta != nil {
			s.delta(b, ev)
		}
	case "content_block_stop":
		if b := s.blocks[ev.Index]; b != nil {
			delete(s.blocks, ev.Index)
			s.stopBlock(b)
		}
	case "message_delta":
		if ev.Delta != nil {
			if ev.Delta.StopReason != "" {
				s.stop = ev.Delta.StopReason
			}
			if d := ev.Delta.StopDetails; d != nil {
				s.detail = d.Category
				if d.Explanation != "" {
					s.detail = strings.TrimSpace(s.detail + ": " + d.Explanation)
				}
			}
		}
		s.usage.add(ev.Usage)
	case "message_stop":
		s.end()
	case "error":
		e := APIError{Provider: s.provider, Message: s.redactor.String(raw.Data)}
		if ev.Error != nil {
			e.Type, e.Message = ev.Error.Type, s.redactor.String(ev.Error.Message)
		}
		return &e
	}
	return nil // ping, and the events added after this was written
}

func (s *anthropicStream) startBlock(index int, raw json.RawMessage) {
	var cb struct {
		Type, ID, Name, Text, Thinking string
	}
	json.Unmarshal(raw, &cb)
	b := &anthropicBlock{kind: cb.Type, start: raw, id: cb.ID}
	s.blocks[index] = b
	switch cb.Type {
	case "text":
		if cb.Text != "" {
			s.queue = append(s.queue, TextDelta{Text: cb.Text})
		}
	case "thinking":
		if cb.Thinking != "" {
			b.text.WriteString(cb.Thinking)
			if !s.notes {
				s.queue = append(s.queue, ReasoningDelta{Text: cb.Thinking})
			}
		}
	case "tool_use":
		s.queue = append(s.queue, ToolCallStart{ID: cb.ID, Name: cb.Name})
	}
}

func (s *anthropicStream) delta(b *anthropicBlock, ev anthropicEvent) {
	d := ev.Delta
	switch d.Type {
	case "text_delta":
		if d.Text != "" {
			s.queue = append(s.queue, TextDelta{Text: d.Text})
		}
	case "thinking_delta":
		if d.Thinking != "" {
			b.text.WriteString(d.Thinking)
			if !s.notes {
				s.queue = append(s.queue, ReasoningDelta{Text: d.Thinking})
			}
		}
	case "signature_delta":
		b.sig.WriteString(d.Signature)
	case "input_json_delta":
		if d.PartialJSON != "" {
			b.input = true
			s.queue = append(s.queue, ToolCallArgsDelta{ID: b.id, Args: d.PartialJSON})
		}
	}
}

// stopBlock ends a block. A thinking block, and any block of a kind the
// adapter doesn't know, is kept as the API sent it, to send it back.
func (s *anthropicStream) stopBlock(b *anthropicBlock) {
	switch b.kind {
	case "text":
		s.queue = append(s.queue, TextEnd{})
	case "tool_use":
		if !b.input {
			s.queue = append(s.queue, ToolCallArgsDelta{ID: b.id, Args: "{}"})
		}
		s.queue = append(s.queue, ToolCallEnd{ID: b.id})
	case "thinking":
		block, _ := json.Marshal(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{"thinking", b.text.String(), b.sig.String()})
		if note := strings.TrimSpace(b.text.String()); s.notes && note != "" && note != interruptedNote {
			s.queue = append(s.queue, Note{Text: note})
		}
		s.queue = append(s.queue, ReasoningEnd{Opaque: block})
	default: // redacted_thinking, whose start holds all of it, and the rest
		if len(b.start) > 0 {
			s.queue = append(s.queue, ReasoningEnd{Opaque: b.start})
		}
	}
}

func (s *anthropicStream) end() {
	if s.usage.seen {
		s.queue = append(s.queue, Usage{Input: s.usage.Input + s.usage.CacheWrite + s.usage.CacheRead, Output: s.usage.Output,
			CacheRead: s.usage.CacheRead, CacheWrite: s.usage.CacheWrite})
	}
	s.queue = append(s.queue, Done{StopReason: anthropicStop(s.stop), Detail: s.detail})
	s.ended = true
}

// readWhole turns a reply that wasn't streamed into the same events.
func (s *anthropicStream) readWhole() {
	data, err := io.ReadAll(io.LimitReader(s.body, maxEventSize))
	if err != nil {
		s.fail(err)
		return
	}
	var msg struct {
		Content     []json.RawMessage `json:"content"`
		StopReason  string            `json:"stop_reason"`
		StopDetails *struct {
			Category string `json:"category"`
		} `json:"stop_details"`
		Usage *usageFields `json:"usage"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		s.fail(fmt.Errorf("malformed response: %v", err))
		return
	}
	for i, raw := range msg.Content {
		s.startBlock(i, raw)
		b := s.blocks[i]
		delete(s.blocks, i)
		var whole struct {
			Text, Thinking, Signature string
			Input                     json.RawMessage
		}
		json.Unmarshal(raw, &whole)
		switch b.kind { // startBlock sent the text and the thinking
		case "thinking":
			b.sig.WriteString(whole.Signature)
		case "tool_use":
			if len(whole.Input) > 0 {
				b.input = true
				s.queue = append(s.queue, ToolCallArgsDelta{ID: b.id, Args: string(whole.Input)})
			}
		}
		s.stopBlock(b)
	}
	s.stop = msg.StopReason
	if msg.StopDetails != nil {
		s.detail = msg.StopDetails.Category
	}
	s.usage.add(msg.Usage)
	s.end()
}

// fail ends the stream with err.
func (s *anthropicStream) fail(err error) {
	var apiErr *APIError
	switch {
	case s.ctx.Err() != nil:
		err = context.Cause(s.ctx) // cancelled by the caller, or the idle timer fired
	case err == io.EOF:
		err = fmt.Errorf("%s: %w", s.provider, ErrIncomplete)
	case !errors.As(err, &apiErr):
		err = s.redactor.Error(fmt.Errorf("reading the response from %s: %w", s.provider, err))
	}
	s.err = err
	s.Close()
}

func anthropicStop(reason string) StopReason {
	switch reason {
	case "end_turn", "stop_sequence", "":
		return StopEnd
	case "max_tokens":
		return StopMaxTokens
	case "tool_use":
		return StopToolUse
	case "refusal":
		return StopRefusal
	}
	return StopReason(reason)
}
