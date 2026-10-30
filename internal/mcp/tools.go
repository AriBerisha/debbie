package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Tool is a tool a server offers.
type Tool struct {
	Name        string
	Title       string
	Description string
	InputSchema json.RawMessage
	headers     []paramHeader // parameters mirrored into HTTP headers
}

// maxPages bounds a paginated list, against a server that never ends it.
const maxPages = 100

// ListTools lists the server's tools. Tools whose definitions this client
// must refuse are left out, each with a warning.
func (c *Client) ListTools(ctx context.Context) (list []Tool, warnings []string, err error) {
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < maxPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.send(ctx, &request{method: "tools/list", params: params})
		if err != nil {
			return nil, nil, fmt.Errorf("tools/list: %w", err)
		}
		var r struct {
			Tools []struct {
				Name        string          `json:"name"`
				Title       string          `json:"title"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &r); err != nil {
			return nil, nil, fmt.Errorf("tools/list: the answer isn't valid: %v", err)
		}
		for _, t := range r.Tools {
			tool := Tool{Name: t.Name, Title: printable(t.Title, 100), Description: t.Description, InputSchema: t.InputSchema}
			if err := c.check(&tool); err != nil {
				warnings = append(warnings, fmt.Sprintf("tool %q left out: %v", printable(t.Name, 64), err))
				continue
			}
			if seen[tool.Name] {
				continue
			}
			seen[tool.Name] = true
			list = append(list, tool)
		}
		if r.NextCursor == "" || r.NextCursor == cursor {
			return list, warnings, nil
		}
		cursor = r.NextCursor
	}
	return list, append(warnings, fmt.Sprintf("the tool list went on past %d pages; the rest is left out", maxPages)), nil
}

// check refuses tool definitions this client can't use: no name, an input
// schema that isn't an object, and, over HTTP, x-mcp-header annotations
// that break the specification's rules, as it requires.
func (c *Client) check(t *Tool) error {
	if t.Name == "" || len(t.Name) > 128 {
		return errors.New("its name is empty or longer than 128 characters")
	}
	var schema map[string]json.RawMessage
	if json.Unmarshal(t.InputSchema, &schema) != nil || schema == nil {
		return errors.New("its inputSchema isn't a JSON object")
	}
	if c.http && !c.legacy {
		headers, err := paramHeaders(t.InputSchema)
		if err != nil {
			return err
		}
		t.headers = headers
	}
	return nil
}

// CallResult is what a tool call returned, as text.
type CallResult struct {
	Text    string
	IsError bool // the tool reports that it failed
}

// CallTool calls a tool with arguments, a JSON object.
func (c *Client) CallTool(ctx context.Context, t Tool, args json.RawMessage) (CallResult, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	r := &request{method: "tools/call", name: t.Name, params: map[string]any{"name": t.Name, "arguments": args}}
	if len(t.headers) > 0 {
		var decoded map[string]any
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.UseNumber()
		dec.Decode(&decoded)
		r.headers = map[string]string{}
		for _, h := range t.headers {
			if v, ok := h.value(decoded); ok {
				r.headers[h.name] = v
			}
		}
	}
	res, err := c.send(ctx, r)
	if err != nil {
		return CallResult{}, err
	}
	return resultText(res)
}

// resultText turns a tool's result into text for the model. Images, audio
// and binary resources are described, not included.
func resultText(res json.RawMessage) (CallResult, error) {
	var r struct {
		Content []struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Data        string `json:"data"`
			MimeType    string `json:"mimeType"`
			URI         string `json:"uri"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Resource    *struct {
				URI      string  `json:"uri"`
				MimeType string  `json:"mimeType"`
				Text     *string `json:"text"`
				Blob     string  `json:"blob"`
			} `json:"resource"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return CallResult{}, fmt.Errorf("the tool's result isn't valid: %v", err)
	}
	var parts []string
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s, %s, %s: not shown]", c.Type, printable(c.MimeType, 60), size(len(c.Data)*3/4)))
		case "resource_link":
			s := fmt.Sprintf("[resource link %s: %s", printable(c.Name, 100), printable(c.URI, 300))
			if c.Description != "" {
				s += " · " + printable(c.Description, 300)
			}
			parts = append(parts, s+"]")
		case "resource":
			switch res := c.Resource; {
			case res == nil:
			case res.Text != nil:
				parts = append(parts, fmt.Sprintf("[resource %s]\n%s", printable(res.URI, 300), *res.Text))
			default:
				parts = append(parts, fmt.Sprintf("[resource %s, %s, %s: binary, not shown]", printable(res.URI, 300), printable(res.MimeType, 60), size(len(res.Blob)*3/4)))
			}
		default:
			parts = append(parts, fmt.Sprintf("[content of type %q: not shown]", printable(c.Type, 40)))
		}
	}
	text := strings.Join(parts, "\n\n")
	if len(parts) == 0 && len(r.StructuredContent) > 0 && string(r.StructuredContent) != "null" {
		text = string(r.StructuredContent)
	}
	return CallResult{Text: text, IsError: r.IsError}, nil
}

func size(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// paramHeader is a tool parameter mirrored into an Mcp-Param-* header.
type paramHeader struct {
	name string   // what follows Mcp-Param-
	path []string // the property names leading to the parameter
}

// paramHeaders finds a schema's x-mcp-header annotations and checks them
// against the specification's rules: a token, unique ignoring case, on a
// string, integer or boolean property reached from the root through
// properties alone.
func paramHeaders(schema json.RawMessage) ([]paramHeader, error) {
	var root any
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	var out []paramHeader
	seen := map[string]bool{}
	var walk func(node any, path []string, reachable bool) error
	walk = func(node any, path []string, reachable bool) error {
		switch n := node.(type) {
		case map[string]any:
			if h, ok := n["x-mcp-header"]; ok {
				name, _ := h.(string)
				typ, _ := n["type"].(string)
				switch {
				case !reachable || len(path) == 0:
					return errors.New("an x-mcp-header annotation is on a schema not reached through properties alone")
				case !isToken(name):
					return fmt.Errorf("x-mcp-header %q isn't a valid header name", printable(fmt.Sprint(h), 40))
				case seen[strings.ToLower(name)]:
					return fmt.Errorf("x-mcp-header %q appears twice", name)
				case typ != "string" && typ != "integer" && typ != "boolean":
					return fmt.Errorf("x-mcp-header %q is on a parameter that isn't a string, integer or boolean", name)
				}
				seen[strings.ToLower(name)] = true
				out = append(out, paramHeader{name: name, path: slices.Clone(path)})
			}
			for key, value := range n {
				if props, ok := value.(map[string]any); ok && key == "properties" {
					for prop, sub := range props {
						if err := walk(sub, append(path, prop), reachable); err != nil {
							return err
						}
					}
					continue
				}
				if key != "x-mcp-header" {
					if err := walk(value, path, false); err != nil {
						return err
					}
				}
			}
		case []any:
			for _, value := range n {
				if err := walk(value, path, false); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, nil, true); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b paramHeader) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

// isToken reports whether s is an HTTP field name (RFC 9110 tchar).
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

// value is the header's value for a call's arguments, if they hold one.
func (h paramHeader) value(args map[string]any) (string, bool) {
	var cur any = args
	for _, p := range h.path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		if cur, ok = m[p]; !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return headerValue(v), true
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	}
	return "", false // null, or not a primitive
}
