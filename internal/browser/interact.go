package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"debbie/internal/config"
	"debbie/internal/prompt"
	"debbie/internal/tools"
)

// element is what the extension says about an element of the page.
type element struct {
	Tag        string `json:"tag"`
	Role       string `json:"role"`
	Name       string `json:"name"`
	Href       string `json:"href"`
	Target     string `json:"target"`
	FormAction string `json:"formAction"`
	Captcha    bool   `json:"captcha"`
	Sensitive  bool   `json:"sensitive"`
	OnScreen   bool   `json:"onScreen"`
	Disabled   bool   `json:"disabled"`
	Page       string `json:"page"` // the page it is on, from describe
}

// String describes the element in a line: its role, its name, and where a
// link goes.
func (e element) String() string {
	s := e.Role
	if s == "" {
		s = e.Tag
	}
	if e.Name != "" {
		s += fmt.Sprintf(" %q", clipText(e.Name, 80))
	}
	if e.Href != "" && e.Role == "link" {
		s += " → " + clipText(e.Href, 100)
	}
	return s
}

// confirmWords are what an element says when clicking it would do
// something the user always confirms, even in yolo (SPEC §20.6): buying,
// paying, subscribing, joining, donating, sending, deleting, posting,
// signing in, up or out, accepting terms, granting access, liking,
// reporting. The extension has the same list (extension/rules.js).
var confirmWords = regexp.MustCompile(`(?i)\b(buy|purchase|pay|payment|order|checkout|check out|subscribe|unsubscribe|join|donate|transfer|send|delete|remove|publish|post|comment|sign up|signup|sign in|signin|sign out|signout|log in|login|log out|logout|register|accept|agree|allow|authori[sz]e|grant|like|dislike|report)\b`)

var linkSeparators = regexp.MustCompile(`[/?&=_.-]+`)

// confirmReason returns the word that makes a click always ask first, ""
// when none does. A link is judged by its address, since its text is
// whatever the page says, such as a video's title; anything else by its
// label and its form's action.
func confirmReason(e element) string {
	text := clipText(e.Name, 100) + " " + e.FormAction
	if e.Role == "link" && e.FormAction == "" {
		text = ""
		if u, err := url.Parse(e.Href); err == nil {
			raw := u.EscapedPath()
			if u.RawQuery != "" {
				raw += "?" + u.RawQuery
			}
			if decoded, err := url.PathUnescape(raw); err == nil {
				text = linkSeparators.ReplaceAllString(decoded, " ")
			}
		}
	}
	if m := confirmWords.FindStringSubmatch(text); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// listed is an element of an interactive read.
type listed struct {
	Ref      string  `json:"ref"`
	Role     string  `json:"role"`
	Name     string  `json:"name"`
	Href     string  `json:"href"`
	Value    *string `json:"value"`
	OnScreen bool    `json:"onScreen"`
	Disabled bool    `json:"disabled"`
}

// interactiveList writes an interactive read for the model, a line per
// element: those on screen first.
func interactiveList(elements []listed, total int) string {
	var b strings.Builder
	for _, e := range elements {
		line := e.Ref + " " + e.Role
		if e.Name != "" {
			line += fmt.Sprintf(" %q", e.Name)
		}
		if e.Href != "" {
			line += " → " + e.Href
		}
		if e.Value != nil {
			line += fmt.Sprintf(" value %q", *e.Value)
		}
		if e.Disabled {
			line += " (disabled)"
		}
		if !e.OnScreen {
			line += " (off screen)"
		}
		b.WriteString(line + "\n")
	}
	if total > len(elements) {
		fmt.Fprintf(&b, "[%d more elements aren't listed: scroll, then read again.]\n", total-len(elements))
	}
	b.WriteString("[Refs last until the page changes: browser_click, browser_hover ref=eN.]")
	return b.String()
}

// targetParams are how the interaction tools name an element: a ref from
// an interactive read, or a point on the latest screenshot.
var targetParams = []tools.Param{
	{Name: "ref", Type: "string", Description: "An element's ref from browser_read mode=interactive, such as e12."},
	{Name: "x", Type: "integer", Description: "Or a point on the latest screenshot: its x, in the screenshot's pixels."},
	{Name: "y", Type: "integer", Description: "And its y."},
}

// target reads the element a call names.
func target(args tools.Args) (map[string]any, string, error) {
	if ref := strings.TrimSpace(args.String("ref")); ref != "" {
		return map[string]any{"ref": ref}, ref, nil
	}
	if args.Has("x") && args.Has("y") {
		x, y := args.Int("x", -1), args.Int("y", -1)
		if x < 0 || y < 0 {
			return nil, "", errors.New("x and y can't be negative")
		}
		return map[string]any{"x": x, "y": y}, fmt.Sprintf("%d,%d", x, y), nil
	}
	return nil, "", errors.New("give ref, from browser_read mode=interactive, or x and y, from the latest screenshot")
}

// describe asks the extension what a target is.
func (a *Access) describe(ctx context.Context, t map[string]any) (element, error) {
	var e element
	res, err := a.call(ctx, "describe", t)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(res, &e); err != nil {
		return e, fmt.Errorf("the extension's answer: %w", err)
	}
	return e, nil
}

// pageResult is what the extension says after acting on a page.
type pageResult struct {
	URL       string  `json:"url"`
	Title     string  `json:"title"`
	Navigated bool    `json:"navigated"`
	Element   element `json:"element"`
	Percent   int     `json:"percent"`
}

// Click is the browser_click tool.
type Click struct{ a *Access }

func (Click) Name() string          { return "browser_click" }
func (Click) Kind() tools.Kind      { return tools.Read }
func (Click) Description() string   { return prompt.Tool("browser_click") }
func (Click) Params() []tools.Param { return targetParams }

// Prepare learns what the click is on, before asking: the checks and the
// question depend on it. A click asks in SAFE unless the user allowed
// clicks on the site for the session; one that would buy, subscribe, post,
// sign in and the like asks in every mode, each time (SPEC §20.6).
func (t Click) Prepare(ctx context.Context, _ *tools.Env, args tools.Args) (*tools.Call, error) {
	if t.a.Session.Mode() == config.ApprovalPlan {
		return nil, errors.New("clicks aren't available in plan mode, which only reads")
	}
	tg, name, err := target(args)
	if err != nil {
		return nil, err
	}
	e, err := t.a.describe(ctx, tg)
	if err != nil {
		return nil, err
	}
	switch {
	case e.Captcha:
		return nil, errors.New("refused: it is part of a CAPTCHA, which the agent never touches; ask the user to solve it")
	case e.Disabled:
		return nil, fmt.Errorf("%s is disabled", e)
	}
	if e.Href != "" && e.Role == "link" {
		if _, err := t.a.checkURL(e.Href); err != nil {
			return nil, fmt.Errorf("the link goes to %s: %w", e.Href, err)
		}
	}
	call := &tools.Call{Kind: tools.Read, Summary: "browser_click " + name + " " + e.String(), Preview: e.String() + "\non " + e.Page}
	reason := confirmReason(e)
	site := siteOf(hostOf(e.Page))
	switch {
	case reason != "":
		call.Ask = fmt.Sprintf("This click would %s. Allow it?", reason)
	case t.a.Mode() == config.WebSafe:
		call.Ask, call.SessionKey, call.SessionFor = "Click this?", "click:"+site, "clicks on "+site
	}
	tg["confirmed"] = reason != "" // the user said yes to just that, or it doesn't run
	call.Run = func(ctx context.Context) (tools.Result, error) {
		res, err := t.a.call(ctx, "click", tg)
		if err != nil {
			return tools.Result{}, err
		}
		var r pageResult
		if err := json.Unmarshal(res, &r); err != nil {
			return tools.Result{}, fmt.Errorf("the extension's answer: %w", err)
		}
		out := "Clicked " + r.Element.String() + "."
		display := "clicked"
		if r.Navigated {
			out += " The page changed."
			display = clipText(r.Title, 60)
		}
		return tools.Result{Output: untrusted(r.URL, "Title: "+r.Title+"\n"+out), Display: display, Tainted: true}, nil
	}
	return call, nil
}

// Hover is the browser_hover tool: it moves the mouse over an element.
type Hover struct{ a *Access }

func (Hover) Name() string          { return "browser_hover" }
func (Hover) Kind() tools.Kind      { return tools.Read }
func (Hover) Description() string   { return prompt.Tool("browser_hover") }
func (Hover) Params() []tools.Param { return targetParams }

func (t Hover) Prepare(_ context.Context, _ *tools.Env, args tools.Args) (*tools.Call, error) {
	tg, name, err := target(args)
	if err != nil {
		return nil, err
	}
	call := &tools.Call{Kind: tools.Read, Summary: "browser_hover " + name}
	call.Run = func(ctx context.Context) (tools.Result, error) {
		res, err := t.a.call(ctx, "hover", tg)
		if err != nil {
			return tools.Result{}, err
		}
		var r pageResult
		if err := json.Unmarshal(res, &r); err != nil {
			return tools.Result{}, fmt.Errorf("the extension's answer: %w", err)
		}
		return tools.Result{Output: untrusted(r.URL, "The mouse is over "+r.Element.String()+"."), Display: clipText(r.Element.String(), 60), Tainted: true}, nil
	}
	return call, nil
}

// Scroll is the browser_scroll tool.
type Scroll struct{ a *Access }

func (Scroll) Name() string        { return "browser_scroll" }
func (Scroll) Kind() tools.Kind    { return tools.Read }
func (Scroll) Description() string { return prompt.Tool("browser_scroll") }
func (Scroll) Params() []tools.Param {
	return []tools.Param{{Name: "direction", Type: "string", Description: "down (the default), up, top or bottom.", Required: false}}
}

func (t Scroll) Prepare(_ context.Context, _ *tools.Env, args tools.Args) (*tools.Call, error) {
	dir := strings.ToLower(strings.TrimSpace(args.String("direction")))
	if dir == "" {
		dir = "down"
	}
	if dir != "up" && dir != "down" && dir != "top" && dir != "bottom" {
		return nil, fmt.Errorf("direction is up, down, top or bottom, not %q", dir)
	}
	call := &tools.Call{Kind: tools.Read, Summary: "browser_scroll " + dir}
	call.Run = func(ctx context.Context) (tools.Result, error) {
		res, err := t.a.call(ctx, "scroll", map[string]string{"direction": dir})
		if err != nil {
			return tools.Result{}, err
		}
		var r pageResult
		if err := json.Unmarshal(res, &r); err != nil {
			return tools.Result{}, fmt.Errorf("the extension's answer: %w", err)
		}
		return tools.Result{Output: fmt.Sprintf("Scrolled %s: now %d%% of the way down the page.", dir, r.Percent),
			Display: fmt.Sprintf("%d%% down", r.Percent)}, nil
	}
	return call, nil
}

// hostOf returns the host of a URL, "" when it has none.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
