package setup

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"debbie/internal/app"
	"debbie/internal/config"
	"debbie/internal/effort"
	"debbie/internal/netguard"
	"debbie/internal/provider"
)

// DefaultContextWindow is suggested when a server doesn't report its
// context size.
const DefaultContextWindow = 32768

var (
	nameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// errNotTrusted ends a wizard whose certificate the user didn't trust.
var errNotTrusted = errors.New("the certificate wasn't trusted, so nothing was saved")

// Wizard asks the user for a provider and checks it (SPEC §6).
type Wizard struct {
	P      Prompter
	Config *config.Config // the providers so far
	Getenv func(string) string
	// AddSecret, if set, learns an API key as soon as it is typed, so it is
	// scrubbed from all output from then on.
	AddSecret func(string)
	// OpenURL, if set, opens a page in the user's browser, such as the one
	// where Claude API keys are made.
	OpenURL func(url string) error
	// URLs replace presets' URLs, by preset ID; tests point them at fakes.
	URLs  map[string]string
	roots *x509.CertPool // stands in for the system roots in tests
}

// Result is a provider the wizard put together.
type Result struct {
	Name        string
	Provider    *config.Provider
	MakeDefault bool
}

// Run walks the user through the steps. With edit set, it changes that
// provider instead, starting from its settings and keeping its name.
func (w *Wizard) Run(ctx context.Context, edit string) (*Result, error) {
	var old *config.Provider
	if edit != "" {
		old = w.Config.Providers[edit]
	}

	// 1. The kind of server.
	kinds := Choice{Question: "Which model provider?"}
	for i, pr := range Presets {
		kinds.Options = append(kinds.Options, Option{Label: pr.Name, Detail: pr.About, Key: pr.ID})
		if old != nil && pr.ID == old.Preset {
			kinds.Default = i
		}
	}
	i, _, err := pick(w.P, kinds)
	if err != nil {
		return nil, err
	}
	preset := Presets[i]
	prov := &config.Provider{
		Preset:               preset.ID,
		Adapter:              preset.Adapter,
		HeaderTimeoutSeconds: config.DefaultHeaderTimeoutSeconds,
		StreamIdleSeconds:    config.DefaultStreamIdleSeconds,
		Options:              config.ProviderOptions{StreamUsage: true},
	}
	if old != nil {
		// Keep what the wizard doesn't ask about.
		kept := *old
		kept.Preset, kept.Adapter, kept.BaseURL, kept.TLSPinSHA256 = prov.Preset, prov.Adapter, "", ""
		kept.APIKey, kept.APIKeyEnv, kept.Model, kept.ContextWindow = "", "", "", 0
		prov = &kept
	}

	if preset.Adapter == config.AdapterAnthropic {
		// Claude: the key, the model and a check; the API says the rest.
		if err := w.claude(ctx, prov, preset, old); err != nil {
			return nil, err
		}
	} else if err := w.server(ctx, prov, preset, old); err != nil {
		return nil, err
	}

	// 6b. When the conversation is compacted by itself (SPEC §11).
	if err := w.askAutoCompact(prov, old); err != nil {
		return nil, err
	}

	// 7. The name, and whether it is the default.
	res := &Result{Name: edit, Provider: prov, MakeDefault: true}
	others := len(w.Config.Providers)
	if edit == "" {
		if res.Name, err = askDefault(w.P, "Name for this provider", w.suggestName(preset, prov.BaseURL), w.validName); err != nil {
			return nil, err
		}
	} else {
		others--
	}
	switch {
	case others == 0:
	case edit != "" && w.Config.DefaultProvider == edit:
	default:
		if res.MakeDefault, err = yesNo(w.P, "Make it the default provider?", edit == ""); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// server asks for a server's URL, certificate and key, checks it, and asks
// whether its model sees images: steps 2 to 6 of SPEC §6.
func (w *Wizard) server(ctx context.Context, prov *config.Provider, preset Preset, old *config.Provider) error {
	// 2. The URL.
	urlDef := w.presetURL(preset)
	if old != nil {
		urlDef = old.BaseURL
	}
	var err error
	if prov.BaseURL, err = w.askURL(urlDef); err != nil {
		return err
	}

	// 3. The certificate, for https (SPEC §3.3).
	if strings.HasPrefix(prov.BaseURL, "https://") {
		if prov.TLSPinSHA256, err = w.trust(ctx, prov.BaseURL); err != nil {
			return err
		}
	}

	// 4. The API key.
	key, err := w.askKey(prov, preset, old)
	if err != nil {
		return err
	}

	// 5. The checks.
	if ok, err := w.probe(ctx, prov, key, preset, old); err != nil || !ok {
		if err == nil {
			err = errors.New("nothing was saved")
		}
		return err
	}

	// 6. Whether the model sees images: a server can't be relied on to
	// say. Only then do the browser tools take screenshots (SPEC §20.4).
	visionDef := old != nil && old.Vision
	prov.Vision, err = yesNo(w.P, "Can "+prov.Model+" see images (a vision model)? Then the browser tools also send it screenshots; "+
		"otherwise they only read a page's text.", visionDef)
	return err
}

// askURL asks for the base URL. Without a scheme it asks which one, since
// falling back to http silently could send a key unencrypted.
func (w *Wizard) askURL(def string) (string, error) {
	for {
		raw, err := askDefault(w.P, "Base URL", def, func(s string) error {
			if s == "" {
				return errors.New("type the server's URL, such as http://192.168.1.20:8080/v1")
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		if !strings.Contains(raw, "://") {
			schemes := []Option{{Label: "https", Detail: "encrypted", Key: "https"},
				{Label: "http", Detail: "not encrypted: only on a network you trust", Key: "http"}}
			i, _, err := pick(w.P, Choice{Question: "Is it http or https?", Options: schemes, Default: -1})
			if err != nil {
				return "", err
			}
			raw = schemes[i].Label + "://" + raw
		}
		if err := checkURL(raw); err != nil {
			w.P.Say("  " + err.Error())
			def = ""
			continue
		}
		return strings.TrimRight(raw, "/"), nil
	}
}

// checkURL accepts http and https URLs with a host and any port.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("not a URL: %v", err)
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("the URL must start with http:// or https://")
	case u.Hostname() == "":
		return errors.New("the URL needs a host")
	case u.User != nil:
		return errors.New("leave the credentials out of the URL; the API key comes next")
	case u.RawQuery != "" || u.Fragment != "":
		return errors.New("the URL can't have a query or a fragment")
	}
	_, err = netguard.EndpointFromURL(raw, "")
	return err
}

// trust is trust on first use (SPEC §3.3): it reads the certificate the
// server presents, without sending a request, and returns the pin for one
// that no trusted authority vouches for, if the user trusts it.
func (w *Wizard) trust(ctx context.Context, rawURL string) (string, error) {
	info, err := readCertificate(ctx, rawURL, w.roots)
	if err != nil {
		return "", err
	}
	if info.Verified {
		w.P.Say("  ✓ the certificate is signed by an authority this computer trusts")
		return "", nil
	}
	showCertificate(w.P, info)
	ok, err := yesNo(w.P, "Trust this certificate?", false)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errNotTrusted
	}
	return info.Pin, nil
}

func readCertificate(ctx context.Context, rawURL string, roots *x509.CertPool) (*netguard.CertInfo, error) {
	ep, err := netguard.EndpointFromURL(rawURL, "")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	info, err := netguard.ProbeCertificate(ctx, ep, netguard.Options{RootCAs: roots})
	if err != nil {
		return nil, fmt.Errorf("can't read the certificate of %s: %w", net.JoinHostPort(ep.Host, ep.Port), err)
	}
	return info, nil
}

// showCertificate describes a certificate for the trust prompt.
func showCertificate(p Prompter, info *netguard.CertInfo) {
	p.Say("  No authority this computer trusts vouches for the server's certificate (" + info.VerifyError + ").")
	p.Say("    Subject  " + info.Subject)
	p.Say("    Issuer   " + info.Issuer)
	p.Say("    Valid    " + info.NotBefore.Format("2006-01-02") + " to " + info.NotAfter.Format("2006-01-02"))
	if names := append(slices.Clone(info.DNSNames), info.IPAddresses...); len(names) > 0 {
		p.Say("    Names    " + strings.Join(names, ", "))
	}
	p.Say("    SHA-256  " + info.Fingerprint)
	p.Say("  Trust it only if this fingerprint matches the one on the server, for example from")
	p.Say("  openssl x509 -noout -fingerprint -sha256 -in <the server's certificate file>.")
}

// askKey asks how the provider gets its API key, sets the provider up
// accordingly, and returns the key to test with.
func (w *Wizard) askKey(prov *config.Provider, preset Preset, old *config.Provider) (string, error) {
	p := w.P
	if preset.Cloud {
		return w.cloudKey(prov, preset, old) // paste it, make one first, or the variable
	}
	if !preset.Cloud {
		had := old != nil && (old.APIKeyEnv != "" || old.APIKey != "")
		need, err := yesNo(p, "Does the server need an API key?", had)
		if err != nil || !need {
			return "", err
		}
	}
	stores := []Option{
		{Label: "In the config file", Detail: "you paste it here; only your account can read the file", Key: "file"},
		{Label: "In an environment variable", Detail: "you set it yourself, before you start " + app.Name, Key: "env"},
	}
	def := 0
	if old != nil && old.APIKeyEnv != "" {
		def = 1
	}
	i, _, err := pick(p, Choice{Question: "Where should the key be kept?", Options: stores, Default: def})
	if err != nil {
		return "", err
	}
	if stores[i].Key == "env" {
		def := preset.KeyEnv
		if old != nil && old.APIKeyEnv != "" {
			def = old.APIKeyEnv
		}
		name, err := askDefault(p, "Environment variable", def, func(s string) error {
			if !envNameRE.MatchString(s) {
				return errors.New("use letters, digits and _, not starting with a digit")
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		prov.APIKeyEnv = name
		key := w.Getenv(name)
		if key == "" {
			p.Say("  " + name + " isn't set here. Set it before you start " + app.Name + "; the tests below go without a key.")
		} else {
			p.Say("  ✓ " + name + " is set")
		}
		return key, nil
	}
	if old != nil && old.APIKey != "" {
		keep, err := yesNo(p, "Keep the key that's stored now?", true)
		if err != nil {
			return "", err
		}
		if keep {
			prov.APIKey = old.APIKey
			return old.APIKey, nil
		}
	}
	for {
		key, err := p.Secret("API key: ")
		if err != nil {
			return "", err
		}
		if key == "" {
			continue
		}
		if w.AddSecret != nil {
			w.AddSecret(key)
		}
		prov.APIKey = key
		p.Say("  The key goes into the config file, which only your account can read.")
		return key, nil
	}
}

// probe runs the checks, filling in the model, the context size and the
// stream-usage option, and reports whether to save.
func (w *Wizard) probe(ctx context.Context, prov *config.Provider, key string, preset Preset, old *config.Provider) (bool, error) {
	p := w.P
	u, _ := url.Parse(prov.BaseURL)
	pr := &prober{name: u.Host, prov: prov, key: key, preset: preset}
	p.Say("Testing " + prov.BaseURL + " ...")
	healthy, err := pr.health(ctx)
	if err != nil {
		p.Say("  ! " + err.Error())
		return yesNo(p, "Save it anyway?", false)
	}
	if healthy {
		p.Say("  ✓ the server is up (/health)")
	}

	models, alt, err := pr.models(ctx)
	if err != nil {
		p.Say("  ! can't list the models: " + err.Error())
		if !healthy {
			if ok, err := yesNo(p, "Save it anyway?", false); err != nil || !ok {
				return false, err
			}
			def := ""
			if old != nil {
				def = old.Model
			}
			prov.Model, err = askDefault(p, "Model", def, nonEmpty("type the model's name"))
			return err == nil, err
		}
	}
	if alt != "" {
		use, err := yesNo(p, "The API answers at "+alt+". Use that URL?", true)
		if err != nil {
			return false, err
		}
		if use {
			prov.BaseURL = alt
		}
	}
	if err := w.pickModel(prov, models, old); err != nil {
		return false, err
	}

	var levels []effort.Level
	if preset.Props {
		if info, err := pr.serverInfo(ctx); err == nil {
			if info.ContextWindow > 0 {
				prov.ContextWindow = info.ContextWindow
				p.Say(fmt.Sprintf("  ✓ context: %d tokens per request (/props)", info.ContextWindow))
			}
			if levels = info.EffortLevels; len(levels) > 0 {
				p.Say("  ✓ reasoning effort levels: " + strings.Join(levelNames(levels), ", "))
			}
		}
	}
	if prov.ContextWindow == 0 {
		for _, m := range models {
			if m.ID == prov.Model && m.ContextWindow > 0 {
				prov.ContextWindow = m.ContextWindow
				p.Say(fmt.Sprintf("  ✓ context: %d tokens", m.ContextWindow))
			}
		}
	}
	if prov.ContextWindow == 0 {
		p.Say("  The server doesn't say how much context the model has.")
		ans, err := askDefault(p, "Context size in tokens", strconv.Itoa(DefaultContextWindow), func(s string) error {
			if n, err := strconv.Atoi(s); err != nil || n < 1024 {
				return errors.New("type a number of tokens, at least 1024")
			}
			return nil
		})
		if err != nil {
			return false, err
		}
		prov.ContextWindow, _ = strconv.Atoi(ans)
	}

	p.Say("  … sending a test request")
	usage, err := pr.streamUsage(ctx)
	if err != nil {
		p.Say("  ! the test request failed: " + err.Error())
		return yesNo(p, "Save it anyway?", false)
	}
	prov.Options.StreamUsage = usage
	if usage {
		p.Say("  ✓ streamed replies report token usage")
	} else {
		p.Say("  ✓ streamed replies work, without token usage, so the status line can't show context use")
	}

	p.Say("  … checking tool calling")
	ok, problem, err := pr.toolCall(ctx, levels)
	switch {
	case err != nil:
		p.Say("  ! the tool-calling check failed: " + err.Error())
		return yesNo(p, "Save it anyway?", true)
	case !ok:
		p.Say("  ! tool calling doesn't work: " + problem + ".")
		if preset.ToolFix != "" {
			p.Say("    Likely fix: " + preset.ToolFix + ".")
		}
		p.Say("    Without it the agent can chat but not read or change files.")
		return yesNo(p, "Save it anyway?", true)
	}
	p.Say("  ✓ tool calling works")
	return true, nil
}

// pickModel chooses the model: the only one, or one the user picks.
func (w *Wizard) pickModel(prov *config.Provider, models []provider.ModelInfo, old *config.Provider) error {
	p := w.P
	switch len(models) {
	case 0:
		def := ""
		if old != nil {
			def = old.Model
		}
		var err error
		prov.Model, err = askDefault(p, "Model", def, nonEmpty("type the model's name"))
		return err
	case 1:
		prov.Model = models[0].ID
		p.Say("  ✓ model: " + prov.Model)
		return nil
	}
	ids := make([]string, len(models))
	def := 0
	for i, m := range models {
		ids[i] = m.ID
		if old != nil && m.ID == old.Model {
			def = i
		}
	}
	i, err := choose(p, "Which model?", ids, def)
	if err != nil {
		return err
	}
	prov.Model = ids[i]
	return nil
}

// suggestName proposes <preset>-<host>, or for a cloud service the
// preset's ID, such as claude, numbered if that is taken.
func (w *Wizard) suggestName(preset Preset, baseURL string) string {
	host := "server"
	if u, err := url.Parse(baseURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	base := strings.NewReplacer(":", "-", "[", "", "]", "").Replace(preset.ID + "-" + host)
	if preset.Cloud {
		base = preset.ID
	}
	name := base
	for n := 2; w.Config.Providers[name] != nil; n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return name
}

func (w *Wizard) validName(s string) error {
	switch {
	case !nameRE.MatchString(s):
		return errors.New("use letters, digits, '.', '-' and '_', starting with a letter or digit")
	case w.Config.Providers[s] != nil:
		return fmt.Errorf("there is already a provider named %s", s)
	}
	return nil
}

func nonEmpty(msg string) func(string) error {
	return func(s string) error {
		if s == "" {
			return errors.New(msg)
		}
		return nil
	}
}

func levelNames(levels []effort.Level) []string {
	names := make([]string, len(levels))
	for i, l := range levels {
		names[i] = string(l)
	}
	return names
}
