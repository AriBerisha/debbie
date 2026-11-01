package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"debbie/internal/agent"
	"debbie/internal/config"
	"debbie/internal/prompt"
	"debbie/internal/provider"
	"debbie/internal/tools"
)

// Access is a session's web access (SPEC §19): its mode, and the browser
// tools and bridge, which exist only while it is on.
type Access struct {
	Session  *agent.Session
	Registry *tools.Registry
	Bridge   *Bridge
	Web      func() config.Web   // the config's web section as it is now
	Redact   func(string) string // removes the configured secrets
	Vision   func() bool         // the model sees images: screenshots are offered
	// Installer makes the installer that /browser install uses; nil for
	// NewInstaller.
	Installer func() (*Installer, error)

	mu      sync.Mutex
	started time.Time // when the bridge started
}

// Mode returns the web access mode.
func (a *Access) Mode() string { return a.Session.Web() }

// SetMode turns web access on or off, or switches between safe and yolo.
// On, the browser tools join the session and the bridge starts; a note
// says when another session holds the browser, which the tools report
// too.
func (a *Access) SetMode(mode string) (note string) {
	a.Session.SetWeb(mode)
	if mode == config.WebOff {
		a.Registry.Remove("browser_")
		a.Bridge.Stop()
		return ""
	}
	if _, ok := a.Registry.Lookup("browser_navigate"); !ok {
		a.Registry.Add(Navigate{a}, Read{a}, Click{a}, Hover{a}, Scroll{a})
	}
	a.Refresh()
	if err := a.start(); err != nil {
		return err.Error()
	}
	return ""
}

// Refresh offers browser_screenshot exactly while web access is on and the
// model sees images; call it after switching providers.
func (a *Access) Refresh() {
	_, has := a.Registry.Lookup("browser_screenshot")
	want := a.Mode() != config.WebOff && a.Vision != nil && a.Vision()
	switch {
	case want && !has:
		a.Registry.Add(Screenshot{a})
	case !want && has:
		a.Registry.Remove("browser_screenshot")
	}
}

// Install writes the extension and its host and registers the host, as
// "browser install" does, for /browser install.
func (a *Access) Install() (*Installed, *Installer, error) {
	newInstaller := a.Installer
	if newInstaller == nil {
		newInstaller = NewInstaller
	}
	in, err := newInstaller()
	if err != nil {
		return nil, nil, err
	}
	got, err := in.Install()
	return got, in, err
}

// Close gives the browser up as the session ends.
func (a *Access) Close() { a.Bridge.Stop() }

func (a *Access) start() error {
	was := a.Bridge.Status().Started
	if err := a.Bridge.Start(); err != nil {
		return err
	}
	if !was {
		a.mu.Lock()
		a.started = time.Now()
		a.mu.Unlock()
	}
	return nil
}

// hostWait is how long a call waits for the host to connect to a bridge
// that just started: the host looks for a session once a second.
const hostWait = 3 * time.Second

// call asks the extension to do something, starting the bridge if another
// session held the browser before.
func (a *Access) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if a.Mode() == config.WebOff {
		return nil, errors.New("web access is off")
	}
	if err := a.start(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	wait := time.Until(a.started.Add(hostWait))
	a.mu.Unlock()
	for wait > 0 && !a.Bridge.Status().Connected {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		wait -= 50 * time.Millisecond
	}
	return a.Bridge.Call(ctx, method, params)
}

// Describe says how web access stands, for /browser.
func (a *Access) Describe() []string {
	mode := a.Mode()
	if mode == config.WebOff {
		return []string{"Web access is off: /web safe turns it on, with the browser tools. " +
			"/browser install sets up the Chrome extension, step by step."}
	}
	lines := []string{"Web access: " + mode + "."}
	st := a.Bridge.Status()
	switch {
	case !st.Started:
		if o, ok := HolderOf(a.Bridge.dir); ok {
			lines = append(lines, (&ErrBusy{Owner: o}).Error()+".")
		} else {
			lines = append(lines, "The bridge isn't running.")
		}
	case st.Connected && st.Extension.Version != "" && !st.Extension.Current():
		lines = append(lines, st.Extension.Label()+" is connected, but runs an older copy of the extension: click the reload button on its card in "+
			"chrome://extensions, or run /browser install, which says when it's done.")
	case st.Connected && st.Extension.Version != "":
		lines = append(lines, fmt.Sprintf("%s is connected (extension %s). It allows: %s.", st.Extension.Label(), st.Extension.Build, siteList(st.Extension.Sites)))
		if st.Extension.Name == "" {
			lines = append(lines, "Name this browser in the extension's popup (its toolbar icon).")
		}
	case st.Connected:
		lines = append(lines, "Chrome is connected.")
	default:
		lines = append(lines, "Waiting for Chrome: is it open, with the extension loaded? /browser install sets it up.")
	}
	if a.Session.Tainted() {
		lines = append(lines, "Web content is in the conversation: in safe mode, edits and commands ask first until /clear.")
	}
	return lines
}

func siteList(sites []string) string {
	if len(sites) == 0 {
		return "no sites"
	}
	return strings.Join(sites, ", ")
}

// checkURL applies the CLI's half of the hard limits (SPEC §19.4, §20.6)
// to a URL the agent would open; the extension applies them again with
// its list of sites.
func (a *Access) checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%q isn't a URL", raw)
	}
	if u.Scheme != "" && u.Scheme != "https" {
		return nil, fmt.Errorf("only https pages open, not %s ones", u.Scheme)
	}
	if !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("%q isn't an absolute https URL", raw)
	}
	if u.User != nil {
		return nil, errors.New("refused: the URL carries a user name or password")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if isLocalHost(host) {
		return nil, fmt.Errorf("refused: %s is on this machine or a private network", host)
	}
	if host == "chromewebstore.google.com" || host == "chrome.google.com" && strings.HasPrefix(u.Path, "/webstore") {
		return nil, errors.New("refused: the agent doesn't open the Chrome Web Store")
	}
	web := a.Web()
	for _, d := range web.BlockDomains {
		if underDomain(host, d) {
			return nil, fmt.Errorf("refused: %s is in web.block_domains", d)
		}
	}
	if a.Redact != nil && a.Redact(raw) != raw {
		return nil, errors.New("refused: the URL holds a secret from your configuration")
	}
	return u, nil
}

// isLocalHost reports whether a host is an address, or a name for this
// machine or the local network: the browser tools never open those.
func isLocalHost(host string) bool {
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return true
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") || !strings.Contains(host, ".")
}

// underDomain reports whether host is domain or one of its subdomains.
func underDomain(host, domain string) bool {
	domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

// onSites reports whether host is one of the extension's sites or under
// one, as the extension judges it (rules.js, allowedSite).
func onSites(host string, sites []string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, s := range sites {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

// siteOf is what "this site for the session" covers: the host without a
// leading "www.".
func siteOf(host string) string {
	return strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(host), "."), "www.")
}

// untrusted wraps web content for the model (SPEC §19.5), escaping any
// copy of the tags inside it.
func untrusted(source, content string) string {
	content = webTag.ReplaceAllString(content, "&lt;$1")
	source = strings.NewReplacer(`"`, "%22", "<", "%3C", ">", "%3E").Replace(source)
	return "<untrusted_web_content source=\"" + source + "\">\n" + content + "\n</untrusted_web_content>"
}

var webTag = regexp.MustCompile(`(?i)<(/?untrusted_web_content)`)

// Navigate is the browser_navigate tool.
type Navigate struct{ a *Access }

func (Navigate) Name() string        { return "browser_navigate" }
func (Navigate) Kind() tools.Kind    { return tools.Read }
func (Navigate) Description() string { return prompt.Tool("browser_navigate") }
func (Navigate) Params() []tools.Param {
	return []tools.Param{{Name: "url", Type: "string", Description: "The https URL to open.", Required: true}}
}

func (t Navigate) Prepare(_ context.Context, _ *tools.Env, args tools.Args) (*tools.Call, error) {
	raw := strings.TrimSpace(args.String("url"))
	u, err := t.a.checkURL(raw)
	if err != nil {
		return nil, err
	}
	call := &tools.Call{Kind: tools.Read, Summary: "browser_navigate " + clipText(u.String(), 100), Preview: u.String()}
	site := siteOf(u.Hostname())
	allowed := false
	for _, d := range t.a.Web().AllowDomains {
		allowed = allowed || underDomain(strings.ToLower(u.Hostname()), d)
	}
	if st := t.a.Bridge.Status(); st.Connected && st.Extension.Version != "" && !onSites(u.Hostname(), st.Extension.Sites) {
		// The extension will ask the user to add the site (SPEC §20.0): the
		// approval prompt, where there is one, says so.
		call.Preview += "\n" + site + " isn't one of the sites the extension allows yet: Chrome will ask you whether to add it."
	}
	switch {
	case config.LooksLikeToken(raw):
		// Asks in every mode, and no answer covers the next one.
		call.Ask = "This address holds what looks like a token. Open it?"
	case t.a.Mode() == config.WebSafe && !allowed:
		call.Ask, call.SessionKey, call.SessionFor = "Open this page?", "web:"+site, site
	}
	call.Run = func(ctx context.Context) (tools.Result, error) {
		res, err := t.a.call(ctx, "navigate", map[string]string{"url": u.String()})
		if err != nil {
			return tools.Result{}, err
		}
		var page struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		}
		json.Unmarshal(res, &page)
		if page.URL == "" {
			return tools.Result{}, errors.New("the page moved to a site the extension doesn't allow")
		}
		return tools.Result{
			Output:  untrusted(page.URL, "Title: "+page.Title) + "\nThe page is open in the agent's tab: browser_read reads it.",
			Display: clipText(page.Title, 60),
			Tainted: true,
		}, nil
	}
	return call, nil
}

// readWindow is the most of a page one browser_read returns, in
// characters: under the output cap (SPEC §8.4), so nothing is cut.
const readWindow = 20000

// Read is the browser_read tool.
type Read struct{ a *Access }

func (Read) Name() string        { return "browser_read" }
func (Read) Kind() tools.Kind    { return tools.Read }
func (Read) Description() string { return prompt.Tool("browser_read") }
func (Read) Params() []tools.Param {
	return []tools.Param{
		{Name: "mode", Type: "string", Description: "text (the default) or links."},
		{Name: "offset", Type: "integer", Description: "The character to start at, to read on."},
	}
}

func (t Read) Prepare(_ context.Context, _ *tools.Env, args tools.Args) (*tools.Call, error) {
	mode := strings.ToLower(strings.TrimSpace(args.String("mode")))
	if mode == "" {
		mode = "text"
	}
	if mode != "text" && mode != "links" && mode != "interactive" {
		return nil, fmt.Errorf("mode is text, links or interactive, not %q", mode)
	}
	offset := args.Int("offset", 0)
	if offset < 0 {
		return nil, errors.New("offset can't be negative")
	}
	summary := "browser_read " + mode
	if offset > 0 {
		summary += fmt.Sprintf(" from %d", offset)
	}
	call := &tools.Call{Kind: tools.Read, Summary: summary}
	call.Run = func(ctx context.Context) (tools.Result, error) {
		res, err := t.a.call(ctx, "read", map[string]string{"mode": mode})
		if err != nil {
			return tools.Result{}, err
		}
		var page struct {
			URL       string `json:"url"`
			Title     string `json:"title"`
			Text      string `json:"text"`
			Truncated bool   `json:"truncated"`
			Links     []struct {
				Text string `json:"text"`
				URL  string `json:"url"`
			} `json:"links"`
			Elements []listed `json:"elements"`
			Total    int      `json:"total"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return tools.Result{}, fmt.Errorf("the extension's answer: %w", err)
		}
		if _, err := t.a.checkURL(page.URL); err != nil {
			return tools.Result{}, fmt.Errorf("the agent's tab is at %s: %w", page.URL, err)
		}
		body, display := page.Text, ""
		if mode == "interactive" {
			body, display = interactiveList(page.Elements, page.Total), tools.Count(len(page.Elements), "element", "elements")
		}
		if mode == "links" {
			var b strings.Builder
			for _, l := range page.Links {
				fmt.Fprintf(&b, "- %s: %s\n", strings.TrimSpace(l.Text), l.URL)
			}
			body, display = b.String(), tools.Count(len(page.Links), "link", "links")
		}
		total := utf8.RuneCountInString(body)
		if offset > total {
			return tools.Result{}, fmt.Errorf("offset %d is past the end: the page has %d characters", offset, total)
		}
		window := []rune(body)[offset:min(total, offset+readWindow)]
		if display == "" {
			display = tools.Count(len(window), "character", "characters")
		}
		out := untrusted(page.URL, "Title: "+page.Title+"\n\n"+string(window))
		if end := offset + len(window); end < total {
			out += fmt.Sprintf("\n[%d more characters: call browser_read with offset=%d to read on.]", total-end, end)
		}
		if page.Truncated {
			out += "\n[The extension sends the first 100,000 characters of a page; the rest isn't available.]"
		}
		return tools.Result{Output: out, Display: display, Tainted: true}, nil
	}
	return call, nil
}

func clipText(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// maxScreenshot is the most a screenshot may weigh.
const maxScreenshot = 5 << 20

// Screenshot is the browser_screenshot tool, offered only to a model that
// sees images (the provider's vision setting).
type Screenshot struct{ a *Access }

func (Screenshot) Name() string          { return "browser_screenshot" }
func (Screenshot) Kind() tools.Kind      { return tools.Read }
func (Screenshot) Description() string   { return prompt.Tool("browser_screenshot") }
func (Screenshot) Params() []tools.Param { return nil }

func (t Screenshot) Prepare(context.Context, *tools.Env, tools.Args) (*tools.Call, error) {
	call := &tools.Call{Kind: tools.Read, Summary: "browser_screenshot"}
	call.Run = func(ctx context.Context) (tools.Result, error) {
		res, err := t.a.call(ctx, "screenshot", nil)
		if err != nil {
			return tools.Result{}, err
		}
		var shot struct {
			URL       string `json:"url"`
			Title     string `json:"title"`
			MediaType string `json:"mediaType"`
			Data      string `json:"data"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		}
		if err := json.Unmarshal(res, &shot); err != nil {
			return tools.Result{}, fmt.Errorf("the extension's answer: %w", err)
		}
		if _, err := t.a.checkURL(shot.URL); err != nil {
			return tools.Result{}, fmt.Errorf("the agent's tab is at %s: %w", shot.URL, err)
		}
		if shot.MediaType != "image/jpeg" && shot.MediaType != "image/png" {
			return tools.Result{}, fmt.Errorf("the screenshot is %q, not a JPEG or PNG image", shot.MediaType)
		}
		data, err := base64.StdEncoding.DecodeString(shot.Data)
		if err != nil || len(data) == 0 {
			return tools.Result{}, errors.New("the screenshot's data isn't valid")
		}
		if len(data) > maxScreenshot {
			return tools.Result{}, fmt.Errorf("the screenshot weighs %d bytes, over the limit of %d", len(data), maxScreenshot)
		}
		return tools.Result{
			Output: untrusted(shot.URL, "Title: "+shot.Title) +
				fmt.Sprintf("\nA screenshot of the agent's tab, %d×%d pixels, comes with this result.", shot.Width, shot.Height),
			Display: fmt.Sprintf("%d×%d", shot.Width, shot.Height),
			Tainted: true,
			Images:  []provider.Image{{MediaType: shot.MediaType, Data: data}},
		}, nil
	}
	return call, nil
}
