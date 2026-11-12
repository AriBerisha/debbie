// Package tools implements what the model can do in the workspace: read,
// write and edit files, list and search them, and run commands (SPEC §8).
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"debbie/internal/provider"
)

// Kind says what a call does, which decides whether it needs approval.
type Kind int

const (
	Read    Kind = iota // reads only
	Edit                // changes files
	Execute             // runs commands
)

// Param describes one argument of a tool.
type Param struct {
	Name        string
	Type        string // "string", "integer" or "boolean"
	Description string
	Required    bool
	// Aliases are other names models send for it, such as Qwen Code's
	// file_path for path.
	Aliases []string
}

// Tool is something the model can call.
type Tool interface {
	Name() string
	Kind() Kind
	Description() string
	Params() []Param
	// Prepare checks the arguments and works out what the call would do,
	// without doing it, so it can be shown and approved first.
	Prepare(ctx context.Context, env *Env, args Args) (*Call, error)
}

// Schemer is a tool that describes its parameters with a JSON schema of
// its own, such as an MCP server's tool, instead of Params. Its arguments
// reach Prepare as the model sent them: ParseArgs only checks that they
// are a JSON object, and the tool's owner checks the rest.
type Schemer interface {
	Schema() json.RawMessage
}

// OutsideReadKey is the session key of every read outside the workspace:
// one "yes for this session" lets the model read and list outside it for
// the rest of the session, since the files there come many at a time.
const OutsideReadKey = "read:outside"

// outsideWrite marks a change to a file outside the workspace: "yes for
// this session" covers the files of its folder, which a task changes
// several of, and SessionFor says so.
func outsideWrite(call *Call, abs string) {
	dir := filepath.Dir(abs)
	call.Outside = []string{abs}
	call.SessionKey, call.SessionFor = "edit-outside:"+trackKey(dir), "files in "+dir
}

// Call is a prepared tool call.
type Call struct {
	Kind    Kind
	Summary string   // one line for the UI, such as "read_file src/main.go"
	Preview string   // what an approval prompt shows: a diff, a command
	Outside []string // paths outside the workspace that the call touches
	// SessionKey is what a "yes for this session" answer covers: the file
	// for an edit, the exact command for the shell, every read outside the
	// workspace for one of them (OutsideReadKey), the folder for a change
	// outside it.
	SessionKey string
	// Ask, when set, is the question of a call that asks whatever the
	// approval mode, such as opening a page in SAFE web mode, unless its
	// SessionKey was allowed. SessionFor names what "yes for this
	// session" then covers, such as a site; "" leaves that answer out. A
	// change outside the workspace sets SessionFor too, to its folder.
	Ask        string
	SessionFor string
	// OnlyLooks says a command seems to change nothing: the agent loop
	// counts it as reading when it watches for a model that never acts.
	OnlyLooks bool
	Run       func(ctx context.Context) (Result, error)
}

// Result is the outcome of a call.
type Result struct {
	Output  string // for the model; the caller caps it
	Display string // a short line for the UI, such as "120 lines"
	Warn    bool   // the call worked but something went wrong, such as a failed command
	// Tainted says the output holds web content, which taints the session
	// (SPEC §19.5).
	Tainted bool
	// Images go to the model with the output, such as a screenshot.
	Images []provider.Image
}

// Env is what the tools of one session share.
type Env struct {
	Workspace *Workspace
	Files     *FileTracker
}

// Registry holds the tools of a session. Tools can join and leave while a
// turn runs, as MCP servers connect.
type Registry struct {
	mu     sync.RWMutex
	tools  []Tool
	byName map[string]Tool
}

// toolAliases maps the names that models trained on Qwen Code use to ours.
var toolAliases = map[string]string{
	"run_shell_command":   "shell",
	"replace":             "edit",
	"grep_search":         "grep",
	"search_file_content": "grep",
	"ls":                  "list_directory",
}

// NewRegistry returns a registry of the given tools.
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{byName: map[string]Tool{}}
	for _, t := range tools {
		r.tools = append(r.tools, t)
		r.byName[t.Name()] = t
	}
	return r
}

// Add adds tools, replacing any with the same name.
func (r *Registry) Add(tools ...Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range tools {
		if _, ok := r.byName[t.Name()]; ok {
			r.remove(func(old Tool) bool { return old.Name() == t.Name() })
		}
		r.tools = append(r.tools, t)
		r.byName[t.Name()] = t
	}
}

// Remove removes the tools whose names start with prefix.
func (r *Registry) Remove(prefix string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remove(func(t Tool) bool { return strings.HasPrefix(t.Name(), prefix) })
}

func (r *Registry) remove(match func(Tool) bool) {
	kept := r.tools[:0]
	for _, t := range r.tools {
		if match(t) {
			delete(r.byName, t.Name())
		} else {
			kept = append(kept, t)
		}
	}
	clear(r.tools[len(kept):])
	r.tools = kept
}

// Lookup finds a tool by name or alias.
func (r *Registry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if t, ok := r.byName[name]; ok {
		return t, true
	}
	t, ok := r.byName[toolAliases[name]]
	return t, ok
}

// Names lists the tools.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, len(r.tools))
	for i, t := range r.tools {
		names[i] = t.Name()
	}
	return names
}

// Specs describes the tools of the given kinds to the model, all of them
// when no kind is given.
func (r *Registry) Specs(kinds ...Kind) []provider.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var specs []provider.ToolSpec
	for _, t := range r.tools {
		if len(kinds) > 0 && !containsKind(kinds, t.Kind()) {
			continue
		}
		spec := provider.ToolSpec{Name: t.Name(), Description: t.Description()}
		if s, ok := t.(Schemer); ok {
			spec.Schema = s.Schema()
		} else {
			spec.Schema = schema(t.Params())
		}
		specs = append(specs, spec)
	}
	return specs
}

func containsKind(kinds []Kind, k Kind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// schema writes a JSON schema for the parameters, keeping their order.
func schema(params []Param) json.RawMessage {
	var b bytes.Buffer
	b.WriteString(`{"type":"object","properties":{`)
	var required []string
	for i, p := range params {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(p.Name)
		prop, _ := json.Marshal(map[string]string{"type": p.Type, "description": p.Description})
		b.Write(name)
		b.WriteByte(':')
		b.Write(prop)
		if p.Required {
			required = append(required, p.Name)
		}
	}
	b.WriteByte('}')
	if len(required) > 0 {
		req, _ := json.Marshal(required)
		b.WriteString(`,"required":`)
		b.Write(req)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// Args are a call's checked arguments: string, int or bool values.
type Args map[string]any

func (a Args) String(name string) string { s, _ := a[name].(string); return s }
func (a Args) Bool(name string) bool     { b, _ := a[name].(bool); return b }
func (a Args) Has(name string) bool      { _, ok := a[name]; return ok }

func (a Args) Int(name string, def int) int {
	if i, ok := a[name].(int); ok {
		return i
	}
	return def
}

// ArgsError reports arguments the model got wrong. Kind is "json" for
// arguments that aren't valid JSON and "params" for wrong parameters.
type ArgsError struct {
	Kind, Msg string
}

func (e *ArgsError) Error() string { return e.Msg }

// ParseArgs decodes a call's JSON arguments and checks them against the
// tool's parameters: names, accepting known aliases; required parameters;
// and types. A number or boolean sent as a string is converted, since small
// models often quote them. Unknown parameters are ignored.
func ParseArgs(t Tool, raw json.RawMessage) (Args, error) {
	in := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&in); err != nil {
			return nil, &ArgsError{Kind: "json", Msg: fmt.Sprintf("the arguments are not a valid JSON object: %v", err)}
		}
	}
	if _, ok := t.(Schemer); ok {
		return Args(in), nil
	}
	out := Args{}
	for _, p := range t.Params() {
		v, ok := in[p.Name]
		for _, alias := range p.Aliases {
			if !ok {
				v, ok = in[alias]
			}
		}
		if !ok || v == nil {
			if p.Required {
				return nil, &ArgsError{Kind: "params", Msg: fmt.Sprintf("missing the required parameter %q", p.Name)}
			}
			continue
		}
		cv, err := coerce(p, v)
		if err != nil {
			return nil, err
		}
		out[p.Name] = cv
	}
	return out, nil
}

func coerce(p Param, v any) (any, error) {
	switch p.Type {
	case "string":
		switch x := v.(type) {
		case string:
			return x, nil
		case json.Number:
			return x.String(), nil
		}
	case "integer":
		switch x := v.(type) {
		case json.Number:
			if f, err := x.Float64(); err == nil && f == math.Trunc(f) && math.Abs(f) < 1<<53 {
				return int(f), nil
			}
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
				return i, nil
			}
		}
	case "boolean":
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			if b, err := strconv.ParseBool(strings.TrimSpace(x)); err == nil {
				return b, nil
			}
		}
	}
	return nil, &ArgsError{Kind: "params", Msg: fmt.Sprintf("the parameter %q must be %s, not %s", p.Name, typeName(p.Type), jsonKind(v))}
}

func typeName(t string) string {
	switch t {
	case "integer":
		return "an integer"
	case "boolean":
		return "true or false"
	}
	return "a string"
}

func jsonKind(v any) string {
	switch x := v.(type) {
	case string:
		return "a string"
	case json.Number:
		return "the number " + x.String()
	case bool:
		return "a boolean"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}
