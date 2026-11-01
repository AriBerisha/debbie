// Package provider defines the neutral message format the agent works with
// and the adapters that translate it to each provider's API.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"debbie/internal/effort"
)

// Role is who wrote a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// PartKind says what a Part holds.
type PartKind int

const (
	PartText PartKind = iota
	PartReasoning
	PartToolCall
	PartToolResult
)

// Message is one turn of the conversation.
type Message struct {
	Role  Role
	Parts []Part
}

// Part is one piece of a message.
type Part struct {
	Kind       PartKind
	Text       string
	ToolCall   *ToolCall
	ToolResult *ToolResult
	// Opaque holds provider data that must be sent back verbatim, such as an
	// Anthropic thinking signature.
	Opaque json.RawMessage
}

// ToolCall is a request from the model to run a tool.
type ToolCall struct {
	ID, Name string
	Args     json.RawMessage
}

// ToolResult is the outcome of a tool call, sent back to the model.
type ToolResult struct {
	CallID, Content string
	IsError         bool
	Images          []Image // pictures that come with it, such as a screenshot
}

// Image is a picture for a model that sees images.
type Image struct {
	MediaType string // "image/jpeg" or "image/png"
	Data      []byte
}

// KeptImages are the images of the conversation sent to the model, the
// newest ones: an older screenshot is left out to save the context, and
// the model is told so.
const KeptImages = 1

// keptImages returns the tool results whose images are sent.
func keptImages(msgs []Message) map[*ToolResult]bool {
	kept := map[*ToolResult]bool{}
	for i := len(msgs) - 1; i >= 0 && len(kept) < KeptImages; i-- {
		for j := len(msgs[i].Parts) - 1; j >= 0 && len(kept) < KeptImages; j-- {
			if r := msgs[i].Parts[j].ToolResult; r != nil && len(r.Images) > 0 {
				kept[r] = true
			}
		}
	}
	return kept
}

// ToolSpec describes a tool to the model.
type ToolSpec struct {
	Name, Description string
	Schema            json.RawMessage
}

// Request is everything sent to the model for one response.
type Request struct {
	System      string
	Messages    []Message
	Tools       []ToolSpec
	MaxTokens   int          // 0 leaves the limit to the server
	Temperature *float64     // nil leaves it to the server
	Effort      effort.Level // effort.Default leaves it to the server
	// ReasoningBudget caps the tokens the model thinks per reply; 0 leaves
	// it to the server. llama-server takes it (SPEC §4.3); other servers
	// would ignore or refuse it, so it is sent only when set.
	ReasoningBudget int
}

// TextMessage returns a message holding one text part.
func TextMessage(role Role, text string) Message {
	return Message{Role: role, Parts: []Part{{Kind: PartText, Text: text}}}
}

// Text returns the text parts of the message joined together, without
// reasoning.
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Kind == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func (m Message) hasToolResults() bool {
	for _, p := range m.Parts {
		if p.Kind == PartToolResult {
			return true
		}
	}
	return false
}

// Event is one item of a streamed response: TextDelta, TextEnd,
// ReasoningDelta, ReasoningEnd, ToolCallStart, ToolCallArgsDelta,
// ToolCallEnd, Usage or Done; and Retrying, from StreamRetry.
type Event interface{ isEvent() }

// TextDelta is the next piece of the answer.
type TextDelta struct{ Text string }

// TextEnd ends a block of text, for a provider whose replies are made of
// blocks: text after it starts a new part, as the provider expects it back.
type TextEnd struct{}

// ReasoningDelta is the next piece of the model's reasoning.
type ReasoningDelta struct{ Text string }

// ReasoningEnd ends a block of reasoning. Opaque, when set, is the block
// as the provider sent it, which goes back to it unchanged in later
// requests: an Anthropic thinking block with its signature (SPEC §4.4).
type ReasoningEnd struct{ Opaque json.RawMessage }

// Retrying says a request failed in a way worth another try, which comes
// after Wait (SPEC §4.5).
type Retrying struct {
	Attempt, Of int // this retry, from 1, and how many there may be
	Wait        time.Duration
	Err         error
}

// ToolCallStart begins a tool call.
type ToolCallStart struct{ ID, Name string }

// ToolCallArgsDelta is the next piece of a tool call's JSON arguments.
type ToolCallArgsDelta struct{ ID, Args string }

// ToolCallEnd completes a tool call.
type ToolCallEnd struct{ ID string }

// Usage reports the tokens a response consumed.
type Usage struct {
	Input, Output int
	// CacheRead and CacheWrite are the input tokens read from and written
	// to the provider's prompt cache; Input includes them.
	CacheRead, CacheWrite int
}

// Modeler is a provider that says which model it asks for; both adapters
// do, and the session prices requests by it.
type Modeler interface {
	Model() string
}

// Done is the last event of a complete response.
type Done struct {
	StopReason StopReason
	// Detail says more about the stop, such as the category of a refusal;
	// "" when there is nothing to add.
	Detail string
}

func (TextDelta) isEvent()         {}
func (TextEnd) isEvent()           {}
func (ReasoningDelta) isEvent()    {}
func (ReasoningEnd) isEvent()      {}
func (Retrying) isEvent()          {}
func (ToolCallStart) isEvent()     {}
func (ToolCallArgsDelta) isEvent() {}
func (ToolCallEnd) isEvent()       {}
func (Usage) isEvent()             {}
func (Done) isEvent()              {}

// StopReason says why the model stopped. Values a provider sends that have
// no constant here are passed through as they are.
type StopReason string

const (
	StopEnd       StopReason = "end"        // the answer is complete
	StopMaxTokens StopReason = "max_tokens" // the output limit was reached
	StopToolUse   StopReason = "tool_use"   // the model wants to call tools
	StopFiltered  StopReason = "filtered"   // a content filter cut the answer
	// StopRefusal is a model or its safety classifiers declining the
	// request: none of the reply's tool calls may run, and what it wrote
	// before declining isn't an answer (SPEC §4.4).
	StopRefusal StopReason = "refusal"
)

// Provider streams responses from one model endpoint.
type Provider interface {
	Stream(ctx context.Context, req Request) (Stream, error)
}

// Stream delivers the events of one response in order.
type Stream interface {
	// Recv returns the next event. After Done it returns io.EOF. Any other
	// error means the response broke off; the events received until then
	// stay valid.
	Recv() (Event, error)
	// Close releases the connection. It may be called at any time, more than
	// once.
	Close() error
}

// ErrIncomplete reports a response that ended before the model finished.
var ErrIncomplete = errors.New("the response ended before it was complete")

// IdleError reports a stream that sent nothing for longer than the
// provider's stream_idle_seconds.
type IdleError struct {
	Provider string
	After    time.Duration
}

func (e *IdleError) Error() string {
	return fmt.Sprintf("%s sent nothing for %s; giving up on the response (stream_idle_seconds)", e.Provider, e.After)
}

// APIError is an error response from a provider.
type APIError struct {
	Provider   string
	Status     int    // HTTP status; 0 for an error sent inside a stream
	Type       string // the provider's kind of error, such as Anthropic's "overloaded_error"; "" when it sends none
	Message    string
	RetryAfter time.Duration // from the Retry-After header; 0 when absent
}

// ModelInfo describes a model a server offers. What the server doesn't
// report is left zero.
type ModelInfo struct {
	ID            string
	Name          string // a display name, such as "Claude Opus 5.5"
	ContextWindow int
	MaxOutput     int  // the most output tokens a reply may have
	Vision        bool // it takes images
	// Adaptive says it thinks with adaptive thinking (Anthropic), and
	// EffortLevels are the effort levels it takes, weakest first.
	Adaptive     bool
	EffortLevels []effort.Level
}

func (e *APIError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("%s reported an error: %s", e.Provider, e.Message)
	}
	return fmt.Sprintf("%s returned HTTP %d %s: %s", e.Provider, e.Status, http.StatusText(e.Status), e.Message)
}
