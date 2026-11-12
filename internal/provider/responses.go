package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"debbie/internal/effort"
)

// OpenAI's Responses API (SPEC §4.6), which the adapter speaks to the
// provider's own servers: its newest models take function tools together
// with reasoning only there, and the Chat Completions parameters they
// reject (max_tokens, reasoning_effort with tools) have one shape here.
// Checked 2026-11-07 against the reference at
// developers.openai.com/api/reference/resources/responses and the
// function-calling, reasoning and streaming guides.

// minResponsesOutputTokens is the smallest max_output_tokens the API takes.
const minResponsesOutputTokens = 16

// responsesIncludeEncrypted asks for the reasoning items' encrypted
// content: with store false they carry it by default now, and older
// models need the request to say so.
const responsesIncludeEncrypted = "reasoning.encrypted_content"

type responsesRequest struct {
	Model        string `json:"model"`
	Instructions string `json:"instructions,omitempty"`
	Input        []any  `json:"input"`
	// Tools are flat here: no "function" wrapper as in Chat Completions.
	Tools  []responsesTool `json:"tools,omitempty"`
	Stream bool            `json:"stream"`
	// Store is false: the conversation stays with the agent, nothing of it
	// with the provider, and reasoning comes back encrypted for the agent
	// to send back (SPEC §4.6).
	Store           bool                `json:"store"`
	Include         []string            `json:"include,omitempty"`
	MaxOutputTokens int                 `json:"max_output_tokens,omitempty"`
	Reasoning       *responsesReasoning `json:"reasoning,omitempty"`
}

type responsesReasoning struct {
	Effort string `json:"effort,omitempty"`
	// Summary asks for a summary of the reasoning to show ("auto": the
	// most detailed one the model has).
	Summary string `json:"summary,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	// Strict is sent as false: left out, the API rewrites the schema into
	// its strict form when it can, which makes every property required.
	// The tools' schemas mean what they say, and the agent checks the
	// arguments itself.
	Strict bool `json:"strict"`
}

// responsesMessage is an input message. Content is a string, or
// []responsesPart when images come with the text.
type responsesMessage struct {
	Type    string `json:"type"` // "message"
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type responsesPart struct {
	Type     string `json:"type"` // "input_text" or "input_image"
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// responsesCall is a tool call the model made, sent back as it was.
type responsesCall struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// responsesCallOutput is a tool's result.
type responsesCallOutput struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// responsesEvent is one streamed event. The fields of every kind of event
// the adapter reads are here; each kind fills its own.
type responsesEvent struct {
	Type        string           `json:"type"`
	ItemID      string           `json:"item_id"`
	OutputIndex int              `json:"output_index"`
	Delta       string           `json:"delta"`
	Arguments   string           `json:"arguments"` // function_call_arguments.done
	Item        json.RawMessage  `json:"item"`      // output_item.added and .done
	Response    *responsesObject `json:"response"`  // created, completed, incomplete, failed
	// An "error" event.
	Code    string `json:"code"`
	Message string `json:"message"`
}

// responsesItem is an output item: a message, a function call or a
// reasoning item.
type responsesItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"` // "output_text" or "refusal"
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

// responsesObject is the response, as the last event and a whole reply
// carry it.
type responsesObject struct {
	Status            string `json:"status"` // completed, incomplete, failed, …
	IncompleteDetails *struct {
		Reason string `json:"reason"` // max_output_tokens, content_filter, …
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Output []json.RawMessage `json:"output"`
	Usage  *struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

// responsesInput turns the conversation into input items. Each assistant
// turn's items go back in the order the model wrote them: its reasoning
// items verbatim, with their encrypted content, for the steps after the
// last message the user typed, which is what the API needs to go on from
// a tool call (older ones it would discard); its text; its function calls.
// A tool's result is a function_call_output; its images follow in a user
// message, the newest only (KeptImages), as in Chat Completions.
func responsesInput(msgs []Message) []any {
	out := make([]any, 0, len(msgs))
	lastUser := -1
	for i, m := range msgs {
		if m.Role == RoleUser && m.Text() != "" && !m.hasToolResults() {
			lastUser = i
		}
	}
	kept := keptImages(msgs)
	for i, m := range msgs {
		if m.Role == RoleAssistant {
			for _, p := range m.Parts {
				switch {
				case p.Kind == PartReasoning && i > lastUser && isReasoningItem(p.Opaque):
					out = append(out, p.Opaque)
				case p.Kind == PartText && strings.TrimSpace(p.Text) != "":
					out = append(out, responsesMessage{Type: "message", Role: "assistant", Content: p.Text})
				case p.Kind == PartToolCall && p.ToolCall != nil:
					args := string(p.ToolCall.Args)
					if args == "" {
						args = "{}"
					}
					out = append(out, responsesCall{Type: "function_call", CallID: p.ToolCall.ID, Name: p.ToolCall.Name, Arguments: args})
				}
			}
			continue
		}
		var parts []responsesPart
		for _, p := range m.Parts {
			if p.Kind != PartToolResult || p.ToolResult == nil {
				continue
			}
			r := p.ToolResult
			content := r.Content
			if r.IsError {
				content = "Error: " + content
			}
			switch {
			case len(r.Images) > 0 && kept[r]:
				parts = append(parts, responsesPart{Type: "input_text", Text: imageNote(r.CallID)})
				for _, img := range r.Images {
					parts = append(parts, responsesPart{Type: "input_image", ImageURL: dataURL(img)})
				}
			case len(r.Images) > 0:
				content += imageLeftOut
			}
			out = append(out, responsesCallOutput{Type: "function_call_output", CallID: r.CallID, Output: content})
		}
		text := m.Text()
		switch {
		case parts != nil:
			if text != "" {
				parts = append(parts, responsesPart{Type: "input_text", Text: text})
			}
			out = append(out, responsesMessage{Type: "message", Role: "user", Content: parts})
		case text != "":
			out = append(out, responsesMessage{Type: "message", Role: "user", Content: text})
		}
	}
	return out
}

// isReasoningItem reports whether an opaque part is a Responses API
// reasoning item, as opposed to one of another provider's (an Anthropic
// thinking block, say, after /provider use), which the API would refuse.
func isReasoningItem(raw json.RawMessage) bool {
	var item struct {
		Type string `json:"type"`
	}
	return len(raw) > 0 && json.Unmarshal(raw, &item) == nil && item.Type == "reasoning"
}

// responsesBody builds the request. summary says whether to ask for a
// reasoning summary (see streamResponses).
func (o *OpenAI) responsesBody(req Request, summary bool) ([]byte, error) {
	body := responsesRequest{
		Model:        o.model,
		Instructions: req.System,
		Input:        responsesInput(req.Messages),
		Stream:       true,
		Store:        false,
		Include:      []string{responsesIncludeEncrypted},
	}
	for _, t := range req.Tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, responsesTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: schema})
	}
	if req.MaxTokens > 0 {
		body.MaxOutputTokens = max(req.MaxTokens, minResponsesOutputTokens)
	}
	// Temperature is left out: the reasoning models refuse it.
	if l := effort.Fit(req.Effort, o.levels()); l != effort.Default || summary {
		body.Reasoning = &responsesReasoning{Effort: string(l)}
		if summary {
			body.Reasoning.Summary = "auto"
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return withExtraBody(data, o.opts.ExtraBody)
}

// streamResponses sends req to POST /responses. The request asks for a
// reasoning summary, to show the model's reasoning as other providers'
// is shown; the API refuses that for some organizations (those not
// verified with OpenAI), so a refused request is sent once more without
// it, and when that one passes, later requests leave it out too.
func (o *OpenAI) streamResponses(ctx context.Context, req Request) (Stream, error) {
	summary := o.summaries()
	s, err := o.postResponses(ctx, req, summary)
	if err == nil || !summary || !refusedRequest(err) {
		return s, err
	}
	s, err = o.postResponses(ctx, req, false)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.noSummary = true
	o.mu.Unlock()
	return s, nil
}

// refusedRequest reports whether the server rejected a request as
// malformed: a 4xx other than a refused key or a rate limit.
func refusedRequest(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status >= 400 && api.Status < 500 &&
		api.Status != http.StatusUnauthorized && api.Status != http.StatusForbidden && api.Status != http.StatusTooManyRequests
}

func (o *OpenAI) summaries() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.noSummary
}

func (o *OpenAI) postResponses(ctx context.Context, req Request, summary bool) (Stream, error) {
	body, err := o.responsesBody(req, summary)
	if err != nil {
		return nil, err
	}
	s, whole, err := o.post(ctx, "/responses", body)
	if err != nil {
		return nil, err
	}
	s.responses = true
	if whole {
		s.readWholeResponse()
	}
	return s, nil
}

// handleResponses takes one streamed event. The event's type is in its
// data, and in the event line of the stream; the data decides.
func (s *openAIStream) handleResponses(ev sseEvent) error {
	if ev.Data == "[DONE]" { // the API doesn't send it, but a compatible server may
		if !s.ended {
			s.finishResponse(nil, "stop")
		}
		return nil
	}
	var e responsesEvent
	if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
		if ev.Type == "error" {
			return &APIError{Provider: s.provider, Message: s.redactor.String(ev.Data)}
		}
		return fmt.Errorf("malformed event: %v", err)
	}
	typ := e.Type
	if typ == "" {
		typ = ev.Type
	}
	switch typ {
	case "error":
		msg := e.Message
		if msg == "" {
			msg = ev.Data
		}
		return &APIError{Provider: s.provider, Type: e.Code, Message: s.redactor.String(msg)}
	case "response.failed":
		msg, code := "the response failed", ""
		if e.Response != nil && e.Response.Error != nil && e.Response.Error.Message != "" {
			msg, code = e.Response.Error.Message, e.Response.Error.Code
		}
		return &APIError{Provider: s.provider, Type: code, Message: s.redactor.String(msg)}
	case "response.output_item.added":
		var item responsesItem
		json.Unmarshal(e.Item, &item)
		switch item.Type {
		case "function_call":
			s.startItemCall(item)
		case "reasoning":
			s.summaryParts = 0
		}
	case "response.output_text.delta", "response.refusal.delta":
		if e.Delta != "" {
			s.queue = append(s.queue, TextDelta{Text: e.Delta})
		}
	case "response.function_call_arguments.delta":
		if c := s.byItem[e.ItemID]; c != nil && e.Delta != "" {
			s.queue = append(s.queue, ToolCallArgsDelta{ID: c.id, Args: e.Delta})
			c.args += len(e.Delta)
		}
	case "response.function_call_arguments.done":
		if c := s.byItem[e.ItemID]; c != nil && c.args == 0 && e.Arguments != "" {
			s.queue = append(s.queue, ToolCallArgsDelta{ID: c.id, Args: e.Arguments})
			c.args = len(e.Arguments)
		}
	case "response.reasoning_summary_part.added":
		if s.summaryParts > 0 {
			s.queue = append(s.queue, ReasoningDelta{Text: "\n\n"})
		}
		s.summaryParts++
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if e.Delta != "" {
			s.queue = append(s.queue, ReasoningDelta{Text: e.Delta})
		}
	case "response.output_item.done":
		var item responsesItem
		json.Unmarshal(e.Item, &item)
		switch item.Type {
		case "function_call":
			c := s.byItem[item.ID]
			if c == nil {
				c = s.startItemCall(item)
			}
			if c.args == 0 && item.Arguments != "" {
				s.queue = append(s.queue, ToolCallArgsDelta{ID: c.id, Args: item.Arguments})
				c.args = len(item.Arguments)
			}
			s.endItemCall(c)
		case "reasoning":
			// The completed item: the one in output_item.added may lack
			// the end of its encrypted content.
			s.queue = append(s.queue, ReasoningEnd{Opaque: e.Item})
		case "message":
			s.queue = append(s.queue, TextEnd{})
		}
	case "response.completed":
		s.finishResponse(e.Response, "stop")
	case "response.incomplete":
		s.finishResponse(e.Response, "length")
	}
	return nil
}

// startItemCall begins a tool call from its output item.
func (s *openAIStream) startItemCall(item responsesItem) *streamCall {
	c := &streamCall{id: item.CallID, name: item.Name, started: true}
	if c.id == "" {
		c.id = item.ID
	}
	if c.id == "" {
		c.id = fmt.Sprintf("call_%d", len(s.calls))
	}
	if s.byItem == nil {
		s.byItem = map[string]*streamCall{}
	}
	if item.ID != "" {
		s.byItem[item.ID] = c
	}
	s.calls = append(s.calls, c)
	s.queue = append(s.queue, ToolCallStart{ID: c.id, Name: c.name})
	return c
}

func (s *openAIStream) endItemCall(c *streamCall) {
	if !c.ended {
		c.ended = true
		s.queue = append(s.queue, ToolCallEnd{ID: c.id})
	}
}

// finishResponse ends the stream as the response says: its usage, and
// why it stopped, in Chat Completions' terms (stopReason).
func (s *openAIStream) finishResponse(r *responsesObject, finish string) {
	if r != nil {
		if u := r.Usage; u != nil {
			s.usage = &Usage{Input: u.InputTokens, Output: u.OutputTokens}
			if u.InputTokensDetails != nil {
				s.usage.CacheRead = u.InputTokensDetails.CachedTokens
			}
		}
		if finish == "length" && r.IncompleteDetails != nil && r.IncompleteDetails.Reason != "max_output_tokens" {
			finish = r.IncompleteDetails.Reason // content_filter, or one with no name here
		}
	}
	if finish == "stop" && len(s.calls) > 0 {
		finish = "tool_calls"
	}
	s.finish = finish
	s.end()
}

// readWholeResponse turns a response that wasn't streamed into events.
func (s *openAIStream) readWholeResponse() {
	data, err := io.ReadAll(io.LimitReader(s.body, maxEventSize))
	if err != nil {
		s.fail(err)
		return
	}
	var r responsesObject
	if err := json.Unmarshal(data, &r); err != nil {
		s.fail(fmt.Errorf("malformed response: %v", err))
		return
	}
	if r.Error != nil && r.Error.Message != "" {
		s.fail(&APIError{Provider: s.provider, Type: r.Error.Code, Message: s.redactor.String(r.Error.Message)})
		return
	}
	for _, raw := range r.Output {
		var item responsesItem
		json.Unmarshal(raw, &item)
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if text := part.Text + part.Refusal; text != "" {
					s.queue = append(s.queue, TextDelta{Text: text})
				}
			}
			s.queue = append(s.queue, TextEnd{})
		case "function_call":
			c := s.startItemCall(item)
			if item.Arguments != "" {
				s.queue = append(s.queue, ToolCallArgsDelta{ID: c.id, Args: item.Arguments})
			}
			s.endItemCall(c)
		case "reasoning":
			for i, part := range item.Summary {
				if i > 0 {
					s.queue = append(s.queue, ReasoningDelta{Text: "\n\n"})
				}
				s.queue = append(s.queue, ReasoningDelta{Text: part.Text})
			}
			s.queue = append(s.queue, ReasoningEnd{Opaque: raw})
		}
	}
	finish := "stop"
	if r.Status == "incomplete" {
		finish = "length"
	}
	s.finishResponse(&r, finish)
}
