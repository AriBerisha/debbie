package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"debbie/internal/app"
	"debbie/internal/config"
	"debbie/internal/effort"
	"debbie/internal/netguard"
)

// maxErrorBody bounds how much of an error response is read.
const maxErrorBody = 64 << 10

// OpenAI talks to an OpenAI-compatible Chat Completions endpoint, such as
// llama-server, vLLM, Ollama, LM Studio, OpenAI or Gemini.
type OpenAI struct {
	name         string
	client       *http.Client
	baseURL      string // without a trailing slash
	model        string
	apiKey       string
	opts         config.ProviderOptions
	streamIdle   time.Duration
	redactor     *config.Redactor
	effortLevels []effort.Level // what the chat template supports; nil when unknown
}

// NewOpenAI returns an adapter for provider p, called name in messages.
// apiKey may be empty, and so may p.Model until SetModel is called.
func NewOpenAI(name string, p *config.Provider, apiKey string) (*OpenAI, error) {
	ep, err := netguard.EndpointFromURL(p.BaseURL, p.TLSPinSHA256)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", name, err)
	}
	client, err := netguard.NewClient(ep, netguard.Options{HeaderTimeout: p.HeaderTimeout(), ProviderName: name})
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", name, err)
	}
	return &OpenAI{
		name:       name,
		client:     client,
		baseURL:    strings.TrimRight(p.BaseURL, "/"),
		model:      p.Model,
		apiKey:     apiKey,
		opts:       p.Options,
		streamIdle: p.StreamIdle(),
		redactor:   config.NewRedactor(apiKey),
	}, nil
}

// Model returns the model that requests ask for.
func (o *OpenAI) Model() string { return o.model }

// SetModel changes the model that requests ask for.
func (o *OpenAI) SetModel(model string) { o.model = model }

// SetEffortLevels records the reasoning-effort levels the model supports,
// weakest first, so requests fit their level to them (see ServerInfo).
func (o *OpenAI) SetEffortLevels(levels []effort.Level) { o.effortLevels = levels }

type chatRequest struct {
	Model              string                     `json:"model"`
	Messages           []chatMessage              `json:"messages"`
	Tools              []chatTool                 `json:"tools,omitempty"`
	Stream             bool                       `json:"stream"`
	StreamOptions      *streamOptions             `json:"stream_options,omitempty"`
	MaxTokens          int                        `json:"max_tokens,omitempty"`
	Temperature        *float64                   `json:"temperature,omitempty"`
	ReasoningEffort    string                     `json:"reasoning_effort,omitempty"`
	ChatTemplateKwargs map[string]json.RawMessage `json:"chat_template_kwargs,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	// Parts, when set, are the content instead of Content: text and
	// images.
	Parts []chatPart `json:"-"`
}

// chatPart is a piece of a message's content.
type chatPart struct {
	Type     string `json:"type"` // "text" or "image_url"
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// MarshalJSON writes Parts as the content when there are any.
func (m chatMessage) MarshalJSON() ([]byte, error) {
	type plain chatMessage
	if m.Parts == nil {
		return json.Marshal(plain(m))
	}
	return json.Marshal(struct {
		plain
		Content []chatPart `json:"content"`
	}{plain(m), m.Parts})
}

func imagePart(img Image) chatPart {
	p := chatPart{Type: "image_url"}
	p.ImageURL = &struct {
		URL string `json:"url"`
	}{"data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)}
	return p
}

type chatTool struct {
	Type     string       `json:"type"` // "function"
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatToolCallDelta is a piece of a streamed tool call. Servers that send a
// call whole use the same shape.
type chatToolCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"` // a JSON string, or an object from some servers
	} `json:"function"`
}

// chatChunk is a streamed chunk, or a whole response when the server didn't
// stream.
type chatChunk struct {
	Choices []struct {
		Index        int       `json:"index"`
		Delta        chatDelta `json:"delta"`
		Message      chatDelta `json:"message"` // in a whole response
		FinishReason string    `json:"finish_reason"`
	} `json:"choices"`
	Usage   *chatUsage      `json:"usage"`
	Timings *llamaTimings   `json:"timings"`
	Error   json.RawMessage `json:"error"`
}

type chatDelta struct {
	Content          string              `json:"content"`
	Refusal          string              `json:"refusal"`
	ReasoningContent string              `json:"reasoning_content"` // llama-server, vLLM, DeepSeek
	Reasoning        string              `json:"reasoning"`         // OpenRouter, newer vLLM
	ToolCalls        []chatToolCallDelta `json:"tool_calls"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// PromptTokensDetails says how much of the prompt a cache served
	// (OpenAI, and servers that follow it).
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// llamaTimings are llama-server's statistics for a response. They arrive
// even when usage isn't requested.
type llamaTimings struct {
	CacheN     int `json:"cache_n"`
	PromptN    int `json:"prompt_n"`
	PredictedN int `json:"predicted_n"`
}

// chatMessages converts the conversation to Chat Completions messages.
// Reasoning is sent back only when sendReasoning is set, and then only for
// the steps after the last message the user typed: the current turn's tool
// calls, which chat templates such as Qwen's render with their reasoning.
func chatMessages(system string, msgs []Message, sendReasoning bool) []chatMessage {
	out := make([]chatMessage, 0, len(msgs)+1)
	if system != "" {
		out = append(out, chatMessage{Role: "system", Content: system})
	}
	lastUser := -1
	for i, m := range msgs {
		if m.Role == RoleUser && m.Text() != "" && !m.hasToolResults() {
			lastUser = i
		}
	}
	kept := keptImages(msgs)
	for i, m := range msgs {
		if m.Role == RoleAssistant {
			cm := chatMessage{Role: "assistant", Content: m.Text()}
			for _, p := range m.Parts {
				switch {
				case p.Kind == PartToolCall && p.ToolCall != nil:
					tc := chatToolCall{ID: p.ToolCall.ID, Type: "function"}
					tc.Function.Name = p.ToolCall.Name
					tc.Function.Arguments = string(p.ToolCall.Args)
					if tc.Function.Arguments == "" {
						tc.Function.Arguments = "{}"
					}
					cm.ToolCalls = append(cm.ToolCalls, tc)
				case p.Kind == PartReasoning && sendReasoning && i > lastUser:
					cm.ReasoningContent += p.Text
				}
			}
			out = append(out, cm)
			continue
		}
		// A user message: tool results become tool messages, their images
		// and any text a user message after them.
		var images []chatPart
		for _, p := range m.Parts {
			if p.Kind == PartToolResult && p.ToolResult != nil {
				r := p.ToolResult
				content := r.Content
				if r.IsError {
					content = "Error: " + content
				}
				switch {
				case len(r.Images) > 0 && kept[r]:
					images = append(images, chatPart{Type: "text", Text: "The image from the tool call " + r.CallID + ":"})
					for _, img := range r.Images {
						images = append(images, imagePart(img))
					}
				case len(r.Images) > 0:
					content += "\n[Its image was left out of the conversation to save context; take a new one to see the page again.]"
				}
				out = append(out, chatMessage{Role: "tool", ToolCallID: r.CallID, Content: content})
			}
		}
		if images != nil {
			out = append(out, chatMessage{Role: "user", Parts: images})
		}
		text := m.Text()
		if text == "" {
			continue
		}
		if last := len(out) - 1; last >= 0 && out[last].Role == "user" {
			// A reply cancelled before it produced anything leaves two user
			// messages in a row, which some chat templates reject.
			if out[last].Parts != nil {
				out[last].Parts = append(out[last].Parts, chatPart{Type: "text", Text: text})
			} else {
				out[last].Content += "\n\n" + text
			}
			continue
		}
		out = append(out, chatMessage{Role: "user", Content: text})
	}
	return out
}

func chatTools(specs []ToolSpec) []chatTool {
	var out []chatTool
	for _, s := range specs {
		out = append(out, chatTool{Type: "function", Function: chatFunction{Name: s.Name, Description: s.Description, Parameters: s.Schema}})
	}
	return out
}

func (o *OpenAI) requestBody(req Request) ([]byte, error) {
	body := chatRequest{
		Model:              o.model,
		Messages:           chatMessages(req.System, req.Messages, o.opts.SendReasoning),
		Tools:              chatTools(req.Tools),
		Stream:             true,
		MaxTokens:          req.MaxTokens,
		Temperature:        req.Temperature,
		ReasoningEffort:    string(effort.Fit(req.Effort, o.effortLevels)),
		ChatTemplateKwargs: o.opts.ChatTemplateKwargs,
	}
	if o.opts.StreamUsage {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	data, err := json.Marshal(body)
	if err != nil || len(o.opts.ExtraBody) == 0 {
		return data, err
	}
	// extra_body adds fields; it never replaces one set above.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for k, v := range o.opts.ExtraBody {
		if _, set := fields[k]; !set {
			fields[k] = v
		}
	}
	return json.Marshal(fields)
}

// Stream sends req and returns the streamed response.
func (o *OpenAI) Stream(ctx context.Context, req Request) (Stream, error) {
	body, err := o.requestBody(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		cancel(nil)
		return nil, err
	}
	o.setHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := o.client.Do(httpReq)
	if err != nil {
		err = o.transportError(ctx, err)
		cancel(nil)
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := o.apiError(resp)
		resp.Body.Close()
		cancel(nil)
		return nil, err
	}

	s := &openAIStream{ctx: ctx, cancel: cancel, body: resp.Body, provider: o.name, redactor: o.redactor}
	if isJSON(resp.Header.Get("Content-Type")) {
		s.readWhole() // the server ignored "stream": true
		return s, nil
	}
	s.idle = newIdleReader(resp.Body, o.streamIdle, func() {
		cancel(&IdleError{Provider: o.name, After: o.streamIdle})
	})
	s.sse = newSSEReader(s.idle)
	return s, nil
}

// Models lists the models the server offers.
func (o *OpenAI) Models(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	o.setHeaders(req)
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, o.transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, o.apiError(resp)
	}
	var body struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"` // vLLM
			Meta        struct {
				NCtx int `json:"n_ctx"` // llama-server
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxEventSize)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s sent a malformed model list: %v", o.name, err)
	}
	models := make([]ModelInfo, 0, len(body.Data))
	for _, m := range body.Data {
		models = append(models, ModelInfo{ID: m.ID, ContextWindow: max(m.MaxModelLen, m.Meta.NCtx)})
	}
	return models, nil
}

// ServerInfo is what llama-server reports about itself.
type ServerInfo struct {
	// ContextWindow is the context of one request slot; 0 when unknown.
	ContextWindow int
	// EffortLevels are the reasoning-effort levels the chat template renders
	// distinctly, weakest first; nil when it has none.
	EffortLevels []effort.Level
}

// ServerInfo reads llama-server's GET /props, at the root of the server
// rather than under /v1. Other servers answer it with an error.
func (o *OpenAI) ServerInfo(ctx context.Context) (ServerInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverRoot(o.baseURL)+"/props", nil)
	if err != nil {
		return ServerInfo{}, err
	}
	o.setHeaders(req)
	resp, err := o.client.Do(req)
	if err != nil {
		return ServerInfo{}, o.transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ServerInfo{}, o.apiError(resp)
	}
	var body struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
		ChatTemplateCaps struct {
			ReasoningEffortLevels []effort.Level `json:"reasoning_effort_levels"`
		} `json:"chat_template_caps"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxEventSize)).Decode(&body); err != nil {
		return ServerInfo{}, fmt.Errorf("%s sent malformed /props: %v", o.name, err)
	}
	return ServerInfo{
		ContextWindow: body.DefaultGenerationSettings.NCtx,
		EffortLevels:  body.ChatTemplateCaps.ReasoningEffortLevels,
	}, nil
}

// serverRoot strips the API version from a base URL such as
// http://host:8084/v1.
// Health asks llama-server or vLLM whether the server is ready to serve:
// GET /health at the server root. Other servers usually answer 404.
func (o *OpenAI) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverRoot(o.baseURL)+"/health", nil)
	if err != nil {
		return err
	}
	o.setHeaders(req)
	resp, err := o.client.Do(req)
	if err != nil {
		return o.transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return o.apiError(resp)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}

func serverRoot(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

func (o *OpenAI) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", app.Name+"/"+app.Version)
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
}

// transportError describes a failed request. A cancelled context is
// returned as its cause.
func (o *OpenAI) transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return o.redactor.Error(fmt.Errorf("%s: %w", o.name, err))
}

func (o *OpenAI) apiError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return &APIError{
		Provider:   o.name,
		Status:     resp.StatusCode,
		Message:    o.redactor.String(errorMessage(data)),
		RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
}

// errorMessage extracts the message from an error body in the shapes that
// OpenAI-compatible servers use, falling back to the raw text.
func errorMessage(data []byte) string {
	var body struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"` // vLLM
	}
	if json.Unmarshal(data, &body) == nil {
		if msg := errorText(body.Error); msg != "" {
			return msg
		}
		if body.Message != "" {
			return body.Message
		}
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "(no message)"
	}
	if len(text) > 500 {
		text = strings.ToValidUTF8(text[:500], "") + "…"
	}
	return text
}

// errorText reads an "error" value: an object with a message, or a string.
func errorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	return string(raw)
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}

type openAIStream struct {
	ctx      context.Context
	cancel   context.CancelCauseFunc
	body     io.ReadCloser
	sse      *sseReader
	idle     *idleReader
	provider string
	redactor *config.Redactor

	queue   []Event
	finish  string // finish_reason, once the server has sent one
	usage   *Usage
	calls   []*streamCall       // tool calls in the order they began
	byIndex map[int]*streamCall // the same, by the server's index
	ended   bool                // Done is queued
	err     error               // returned once the queue is empty
	closed  bool
}

// streamCall is a tool call being assembled from a stream.
type streamCall struct {
	id, name string
	started  bool            // ToolCallStart has been queued
	pending  strings.Builder // arguments that arrived before the name
}

func (s *openAIStream) Recv() (Event, error) {
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

func (s *openAIStream) Close() error {
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

func (s *openAIStream) handle(ev sseEvent) error {
	if ev.Data == "[DONE]" {
		s.end()
		return nil
	}
	var chunk chatChunk
	if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
		if ev.Type == "error" {
			return &APIError{Provider: s.provider, Message: s.redactor.String(ev.Data)}
		}
		return fmt.Errorf("malformed chunk: %v", err)
	}
	if msg := errorText(chunk.Error); msg != "" || ev.Type == "error" {
		if msg == "" {
			msg = ev.Data
		}
		return &APIError{Provider: s.provider, Message: s.redactor.String(msg)}
	}
	for _, c := range chunk.Choices {
		if c.Index != 0 {
			continue // only one choice is ever requested
		}
		s.addDelta(c.Delta)
		if c.FinishReason != "" {
			s.finish = c.FinishReason
		}
	}
	s.addUsage(chunk)
	return nil
}

// readWhole turns a non-streamed response into events.
func (s *openAIStream) readWhole() {
	data, err := io.ReadAll(io.LimitReader(s.body, maxEventSize))
	if err != nil {
		s.fail(err)
		return
	}
	var chunk chatChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		s.fail(fmt.Errorf("malformed response: %v", err))
		return
	}
	if msg := errorText(chunk.Error); msg != "" {
		s.fail(&APIError{Provider: s.provider, Message: s.redactor.String(msg)})
		return
	}
	for _, c := range chunk.Choices {
		if c.Index == 0 {
			s.addDelta(c.Message)
			s.finish = c.FinishReason
		}
	}
	s.addUsage(chunk)
	s.end()
}

func (s *openAIStream) addDelta(d chatDelta) {
	reasoning := d.ReasoningContent
	if reasoning == "" {
		reasoning = d.Reasoning // some servers send both, with the same text
	}
	if reasoning != "" {
		s.queue = append(s.queue, ReasoningDelta{Text: reasoning})
	}
	if d.Content != "" {
		s.queue = append(s.queue, TextDelta{Text: d.Content})
	}
	if d.Refusal != "" {
		s.queue = append(s.queue, TextDelta{Text: d.Refusal})
	}
	for _, tc := range d.ToolCalls {
		s.addToolCall(tc)
	}
}

// addToolCall takes one piece of a tool call. Pieces carry the server's
// index; the id and name usually come with the first piece and the
// arguments in string fragments to concatenate.
func (s *openAIStream) addToolCall(d chatToolCallDelta) {
	c := s.findCall(d)
	if c.id == "" {
		c.id = d.ID
	}
	if c.name == "" {
		c.name = d.Function.Name
	}
	args := argumentText(d.Function.Arguments)
	if !c.started {
		c.pending.WriteString(args)
		if c.name == "" {
			return
		}
		if c.id == "" {
			c.id = fmt.Sprintf("call_%d", len(s.calls)-1)
		}
		c.started = true
		s.queue = append(s.queue, ToolCallStart{ID: c.id, Name: c.name})
		args = c.pending.String()
	}
	if args != "" {
		s.queue = append(s.queue, ToolCallArgsDelta{ID: c.id, Args: args})
	}
}

func (s *openAIStream) findCall(d chatToolCallDelta) *streamCall {
	if d.Index != nil {
		if c, ok := s.byIndex[*d.Index]; ok {
			return c
		}
		c := &streamCall{}
		if s.byIndex == nil {
			s.byIndex = map[int]*streamCall{}
		}
		s.byIndex[*d.Index] = c
		s.calls = append(s.calls, c)
		return c
	}
	// Without an index, a new id starts a new call.
	if n := len(s.calls); n > 0 && (d.ID == "" || s.calls[n-1].id == d.ID) {
		return s.calls[n-1]
	}
	c := &streamCall{}
	s.calls = append(s.calls, c)
	return c
}

// argumentText returns a piece of tool-call arguments: the text of a JSON
// string, or the raw JSON of an object, which some servers send instead.
func argumentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// addUsage records usage, preferring the usage chunk over llama-server's
// timings.
func (s *openAIStream) addUsage(chunk chatChunk) {
	switch {
	case chunk.Usage != nil:
		s.usage = &Usage{Input: chunk.Usage.PromptTokens, Output: chunk.Usage.CompletionTokens}
		if d := chunk.Usage.PromptTokensDetails; d != nil {
			s.usage.CacheRead = d.CachedTokens
		}
	case chunk.Timings != nil && s.usage == nil:
		t := chunk.Timings
		s.usage = &Usage{Input: t.CacheN + t.PromptN, Output: t.PredictedN, CacheRead: t.CacheN}
	}
}

func (s *openAIStream) end() {
	for _, c := range s.calls {
		if c.started {
			s.queue = append(s.queue, ToolCallEnd{ID: c.id})
		}
	}
	if s.usage != nil {
		s.queue = append(s.queue, *s.usage)
	}
	s.queue = append(s.queue, Done{StopReason: stopReason(s.finish)})
	s.ended = true
}

// fail ends the stream with err, unless the server merely left out [DONE].
func (s *openAIStream) fail(err error) {
	var apiErr *APIError
	switch {
	case s.ctx.Err() != nil:
		err = context.Cause(s.ctx) // cancelled by the caller, or the idle timer fired
	case err == io.EOF && s.finish != "":
		s.end()
		return
	case err == io.EOF:
		err = fmt.Errorf("%s: %w", s.provider, ErrIncomplete)
	case !errors.As(err, &apiErr):
		err = s.redactor.Error(fmt.Errorf("reading the response from %s: %w", s.provider, err))
	}
	s.err = err
	s.Close()
}

func stopReason(finish string) StopReason {
	switch finish {
	case "stop", "":
		return StopEnd
	case "length":
		return StopMaxTokens
	case "tool_calls", "function_call":
		return StopToolUse
	case "content_filter":
		return StopFiltered
	}
	return StopReason(finish)
}
