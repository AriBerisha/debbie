package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"debbie/internal/app"
	"debbie/internal/config"
	"debbie/internal/effort"
	"debbie/internal/provider"
)

// Claude's setup is the easy one (SPEC §6.1): no URL to type, a key
// pasted in, the browser at most to get one, then a model from the list.
// The models API says the rest: the context, the output limit, whether
// the model sees images and the effort levels it takes.
const (
	// claudeKeysURL is the Claude Console's page for API keys.
	claudeKeysURL = "https://platform.claude.com/settings/keys"
	// claudeWorkspacesURL lists the workspaces and their IDs.
	claudeWorkspacesURL = "https://platform.claude.com/settings/workspaces"
	// claudeDefaultModel is the model the wizard suggests.
	claudeDefaultModel = "claude-opus-5-5"
	// claudeMaxOutput is the output limit the wizard sets, at most the
	// model's: room for long answers, which arrive streamed.
	claudeMaxOutput = 64000
	// claudeEffort is the effort a new Claude provider starts with: coding
	// wants the model to think, and /effort changes it.
	claudeEffort = effort.High
)

// claude asks what a Claude provider needs and checks it.
func (w *Wizard) claude(ctx context.Context, prov *config.Provider, preset Preset, old *config.Provider) error {
	p := w.P
	prov.BaseURL = w.presetURL(preset)
	if old != nil && old.BaseURL != "" {
		prov.BaseURL = old.BaseURL // a proxy set up with provider add --url stays
	}
	if strings.HasPrefix(prov.BaseURL, "https://") {
		var err error
		if prov.TLSPinSHA256, err = w.trust(ctx, prov.BaseURL); err != nil {
			return err
		}
	}
	if old != nil {
		prov.Options.WorkspaceID = old.Options.WorkspaceID
	}
	key, err := w.cloudKey(prov, preset, old)
	if err != nil {
		return err
	}
	models, err := w.claudeModels(ctx, prov, preset, &key)
	if err != nil {
		return err
	}
	info, err := w.claudeModel(prov, models, old)
	if err != nil {
		return err
	}
	fillClaude(prov, info, old)
	p.Say(fmt.Sprintf("  ✓ %s: %s context, replies up to %s tokens", prov.Model, kTokens(prov.ContextWindow), kTokens(prov.MaxOutputTokens)))

	p.Say("  … sending a test request")
	ok, problem, err := claudeCheck(ctx, "claude", prov, key, info)
	switch {
	case err != nil:
		p.Say("  ! the test request failed: " + err.Error())
		if save, err := yesNo(p, "Save it anyway?", false); err != nil || !save {
			if err == nil {
				err = errors.New("nothing was saved")
			}
			return err
		}
	case !ok:
		p.Say("  ! " + problem + ".")
	default:
		p.Say("  ✓ Claude answers, and calls tools")
	}
	return nil
}

// presetURL is the URL a preset suggests, or the one tests put there.
func (w *Wizard) presetURL(preset Preset) string {
	if u := w.URLs[preset.ID]; u != "" {
		return u
	}
	return preset.URL
}

// cloudKey asks how to get a cloud service's key: pasted in, perhaps
// after opening the page that makes one, or from the environment
// variable. It returns the key to test with.
func (w *Wizard) cloudKey(prov *config.Provider, preset Preset, old *config.Provider) (string, error) {
	p := w.P
	keyEnv := preset.KeyEnv
	if old != nil && old.APIKeyEnv != "" {
		keyEnv = old.APIKeyEnv
	}
	envKey := w.Getenv(keyEnv)
	type way int
	const (
		keep way = iota
		paste
		browse
		env
	)
	var ways []way
	var c Choice
	c.Question = preset.Name + " needs an API key. How do you want to give it?"
	if old != nil && old.APIKey != "" {
		ways = append(ways, keep)
		c.Options = append(c.Options, Option{Label: "Keep the key that's stored now", Key: "keep"})
	}
	ways = append(ways, paste)
	c.Options = append(c.Options, Option{Label: "Paste a key", Detail: "saved in the config file, which only your account can read", Key: "file"})
	if w.OpenURL != nil && preset.KeysURL != "" {
		ways = append(ways, browse)
		c.Options = append(c.Options, Option{Label: "Make a key first", Detail: "opens " + preset.KeysURL + " in your browser", Key: "browser"})
	}
	ways = append(ways, env)
	envState := "not set here"
	if envKey != "" {
		envState = "it is set"
	}
	c.Options = append(c.Options, Option{Label: "Read it from " + keyEnv, Detail: envState, Key: "env"})
	switch {
	case old != nil && old.APIKey != "":
		c.Default = 0 // the key stored now
	case old != nil && old.APIKeyEnv != "", envKey != "":
		c.Default = len(c.Options) - 1 // already set up: the obvious choice
	}
	i, _, err := pick(p, c)
	if err != nil {
		return "", err
	}
	switch ways[i] {
	case keep:
		prov.APIKey = old.APIKey
		return old.APIKey, nil
	case env:
		if envKey != "" {
			prov.APIKeyEnv = keyEnv
			p.Say("  ✓ " + keyEnv + " is set")
			return envKey, nil
		}
		// Without it, the service can't be checked, and every request
		// fails until the variable is set where debbie starts.
		p.Say("  ! " + keyEnv + " isn't set here, so " + preset.Name + " can't be checked now, and requests fail until it is set where " + app.Name + " starts.")
		paste, err := yesNo(p, "Paste the key now instead?", true)
		if err != nil {
			return "", err
		}
		if !paste {
			prov.APIKeyEnv = keyEnv
			return "", nil
		}
	case browse:
		if err := w.OpenURL(preset.KeysURL); err != nil {
			p.Say("  ! can't open a browser here (" + err.Error() + "). Open " + preset.KeysURL + " yourself.")
		} else {
			p.Say("  Opened " + preset.KeysURL + " in your browser.")
		}
		p.Say("  Make a key there and copy it: most consoles show it only once.")
	}
	key, err := w.askCloudKey(preset)
	if err != nil {
		return "", err
	}
	prov.APIKey = key
	return key, nil
}

// askCloudKey reads a pasted key.
func (w *Wizard) askCloudKey(preset Preset) (string, error) {
	prompt := "Paste the API key: "
	if preset.KeyPrefix != "" {
		prompt = "Paste the API key (it starts with " + preset.KeyPrefix + "): "
	}
	for {
		key, err := w.P.Secret(prompt)
		if err != nil {
			return "", err
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if w.AddSecret != nil {
			w.AddSecret(key)
		}
		if preset.KeyPrefix != "" && !strings.HasPrefix(key, preset.KeyPrefix) {
			w.P.Say("  ! that doesn't look like a " + preset.Name + " API key, which starts with " + preset.KeyPrefix + "; trying it anyway")
		}
		return key, nil
	}
}

// claudeModels lists the models, which also checks the key: a key Claude
// refuses can be pasted again, and one that needs a workspace gets one.
// When Claude can't be reached, the user may name a model and save.
func (w *Wizard) claudeModels(ctx context.Context, prov *config.Provider, preset Preset, key *string) ([]provider.ModelInfo, error) {
	p := w.P
	for {
		models, err := claudeModels(ctx, prov, *key)
		var apiErr *provider.APIError
		switch {
		case err == nil && len(models) > 0:
			p.Say(fmt.Sprintf("  ✓ the key works; %d models", len(models)))
			return models, nil
		case err == nil:
			return nil, nil
		case needsWorkspace(err):
			id, err := w.claudeWorkspace(ctx, prov, *key)
			if err != nil {
				return nil, err
			}
			prov.Options.WorkspaceID = id
			continue
		case errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden) && prov.APIKeyEnv == "":
			p.Say("  ! Claude refused the key: " + apiErr.Message)
			k, err := w.askCloudKey(preset)
			if err != nil {
				return nil, err
			}
			*key, prov.APIKey = k, k
			continue
		}
		p.Say("  ! can't list the models: " + err.Error())
		if save, err := yesNo(p, "Save it anyway?", false); err != nil || !save {
			if err == nil {
				err = errors.New("nothing was saved")
			}
			return nil, err
		}
		return nil, nil
	}
}

// claudeWorkspace asks which workspace requests run in, for a key that
// isn't scoped to one: from the list, if the key may read it, else typed.
func (w *Wizard) claudeWorkspace(ctx context.Context, prov *config.Provider, key string) (string, error) {
	p := w.P
	p.Say("  This key isn't tied to one workspace, so every request has to name one.")
	a, err := provider.NewAnthropic("claude", prov, key)
	if err != nil {
		return "", err
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	spaces, err := a.Workspaces(lctx)
	cancel()
	if err == nil && len(spaces) > 0 {
		c := Choice{Question: "Which workspace?"}
		for i, s := range spaces {
			c.Options = append(c.Options, Option{Label: s.Name, Detail: s.ID})
			if s.Name == "Default" {
				c.Default = i
			}
		}
		i, _, err := pick(p, c)
		if err != nil {
			return "", err
		}
		return spaces[i].ID, nil
	}
	p.Say("  Its ID is in the ID column of " + claudeWorkspacesURL + " (or make a key tied to one workspace).")
	return askDefault(p, "Workspace ID", "", func(s string) error {
		if !strings.HasPrefix(s, "wrkspc_") {
			return errors.New("a workspace ID starts with wrkspc_")
		}
		return nil
	})
}

// claudeModel picks the model from the list, or one typed in.
func (w *Wizard) claudeModel(prov *config.Provider, models []provider.ModelInfo, old *config.Provider) (provider.ModelInfo, error) {
	want := claudeDefaultModel
	if old != nil && old.Model != "" {
		want = old.Model
	}
	if len(models) == 0 {
		id, err := askDefault(w.P, "Model", want, nonEmpty("type the model's ID, such as "+claudeDefaultModel))
		return provider.ModelInfo{ID: id}, err
	}
	c := Choice{Question: "Which model?", Other: "Another model, by its ID"}
	for i, m := range models {
		label := m.Name
		if label == "" {
			label = m.ID
		}
		detail := m.ID
		if m.ContextWindow > 0 {
			detail += " · " + kTokens(m.ContextWindow) + " context"
		}
		c.Options = append(c.Options, Option{Label: label, Detail: detail, Key: m.ID})
		if m.ID == want {
			c.Default = i
		}
	}
	i, typed, err := pick(w.P, c)
	if err != nil {
		return provider.ModelInfo{}, err
	}
	if i < 0 {
		for _, m := range models {
			if m.ID == typed {
				return m, nil
			}
		}
		return provider.ModelInfo{ID: typed}, nil
	}
	return models[i], nil
}

// fillClaude sets what the model supports. A model the API didn't
// describe gets Claude's usual: every Claude model sees images.
func fillClaude(prov *config.Provider, info provider.ModelInfo, old *config.Provider) {
	prov.Model = info.ID
	prov.ContextWindow = info.ContextWindow
	prov.MaxOutputTokens = claudeMaxOutput
	if info.MaxOutput > 0 {
		prov.MaxOutputTokens = min(claudeMaxOutput, info.MaxOutput)
	}
	prov.Vision = info.Vision || info.Name == ""
	switch {
	case old != nil && old.ReasoningEffort != "":
		prov.ReasoningEffort = old.ReasoningEffort
	case slices.Contains(info.EffortLevels, claudeEffort):
		prov.ReasoningEffort = string(claudeEffort)
	}
}

// claudeModels asks the models API.
func claudeModels(ctx context.Context, prov *config.Provider, key string) ([]provider.ModelInfo, error) {
	a, err := provider.NewAnthropic("claude", prov, key)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return a.Models(ctx)
}

// needsWorkspace reports Claude asking for the anthropic-workspace-id
// header: the key isn't scoped to one workspace.
func needsWorkspace(err error) bool {
	var apiErr *provider.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest && strings.Contains(apiErr.Message, "anthropic-workspace-id")
}

// claudeCheck sends one small request offering get_time with a question
// only it answers, at low effort: it shows that the key, the model and
// tool calls all work. ok is false, with the problem, when the model
// answers without the tool.
func claudeCheck(ctx context.Context, name string, prov *config.Provider, key string, info provider.ModelInfo) (ok bool, problem string, err error) {
	a, err := provider.NewAnthropic(name, prov, key)
	if err != nil {
		return false, "", err
	}
	if info.Name != "" {
		a.SetCapabilities(info)
	}
	req := provider.Request{
		Messages:  []provider.Message{provider.TextMessage(provider.RoleUser, "What time is it right now? Use the get_time tool to find out.")},
		Tools:     []provider.ToolSpec{getTime},
		MaxTokens: 4096,
		Effort:    effort.Low,
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	stream, err := provider.StreamRetry(ctx, a, req, nil)
	if err != nil {
		return false, "", err
	}
	defer stream.Close()
	stop, detail := provider.StopEnd, ""
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, "", err
		}
		switch e := ev.(type) {
		case provider.ToolCallStart:
			if e.Name == getTime.Name {
				return true, "", nil
			}
			return false, fmt.Sprintf("the model called %q, a tool it wasn't offered", e.Name), nil
		case provider.Done:
			stop, detail = e.StopReason, e.Detail
		}
	}
	switch stop {
	case provider.StopRefusal:
		return false, "the model declined the test request (" + detail + ")", nil
	case provider.StopMaxTokens:
		return false, "the reply reached the output limit before it called the tool", nil
	}
	return false, "the model answered in plain text instead of calling the tool", nil
}

// connectClaude prepares a Claude provider for a session: it asks the
// models API what the model supports. Without an answer the adapter goes
// by the model's name.
func connectClaude(ctx context.Context, c *Conn, p config.Provider) (*Conn, error) {
	if p.Model == "" {
		return nil, fmt.Errorf("provider %s names no model; /provider edit %s picks one", c.Name, c.Name)
	}
	a, err := provider.NewAnthropic(c.Name, &p, c.APIKey)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	info, err := a.Describe(dctx, p.Model)
	cancel()
	var noKey *provider.NoKeyError
	switch {
	case err == nil:
		a.SetCapabilities(info)
	case errors.As(err, &noKey):
		c.Warnings = append(c.Warnings, NoKeyText(noKey))
	case ctx.Err() == nil:
		c.Warnings = append(c.Warnings, "can't ask Claude what "+p.Model+" supports ("+err.Error()+"); going by its name")
	}
	c.ContextWindow = p.ContextWindow
	if c.ContextWindow == 0 {
		c.ContextWindow = info.ContextWindow
	}
	c.EffortLevels = a.EffortLevels()
	c.Provider, c.Adapter = p, a
	return c, nil
}

// testClaude repeats the checks for /provider test.
func testClaude(ctx context.Context, p Prompter, name string, conn *Conn) error {
	prov := conn.Provider
	p.Say("Testing " + name + " (" + prov.BaseURL + ") ...")
	models, err := claudeModels(ctx, &prov, conn.APIKey)
	var info provider.ModelInfo
	switch {
	case err != nil:
		p.Say("  ! can't list the models: " + err.Error())
	default:
		i := slices.IndexFunc(models, func(m provider.ModelInfo) bool { return m.ID == prov.Model })
		if i < 0 {
			p.Say("  ! Claude doesn't list the model " + prov.Model)
		} else {
			info = models[i]
			p.Say("  ✓ the key works; model: " + prov.Model)
		}
	}
	if conn.ContextWindow > 0 {
		p.Say("  ✓ context: " + kTokens(conn.ContextWindow) + " tokens")
	}
	ok, problem, err := claudeCheck(ctx, name, &prov, conn.APIKey, info)
	switch {
	case err != nil:
		p.Say("  ! the test request failed: " + err.Error())
	case !ok:
		p.Say("  ! " + problem + ".")
	default:
		p.Say("  ✓ Claude answers, and calls tools")
	}
	return nil
}

// addClaudeFromFlags finishes provider add for Claude: the key from
// --api-key-env, the model from --model or the suggested one, the rest
// from the models API, then the check.
func addClaudeFromFlags(ctx context.Context, prov *config.Provider, key string, say func(string, ...any)) error {
	models, err := claudeModels(ctx, prov, key)
	if err != nil {
		if needsWorkspace(err) {
			return errors.New("the key isn't scoped to a workspace; make one that is, or add the provider with the wizard, which asks for the workspace")
		}
		return fmt.Errorf("can't list the models: %w", err)
	}
	if prov.Model == "" {
		prov.Model = claudeDefaultModel
	}
	i := slices.IndexFunc(models, func(m provider.ModelInfo) bool { return m.ID == prov.Model })
	if i < 0 {
		ids := make([]string, len(models))
		for j, m := range models {
			ids[j] = m.ID
		}
		return fmt.Errorf("Claude has no model %q; it has %s", prov.Model, strings.Join(ids, ", "))
	}
	fillClaude(prov, models[i], nil)
	ok, problem, err := claudeCheck(ctx, "claude", prov, key, models[i])
	switch {
	case err != nil:
		return fmt.Errorf("the test request failed: %w", err)
	case !ok:
		say("warning: %s.", problem)
	}
	return nil
}

// NoKeyText says that a provider has no API key, and how to give it one.
func NoKeyText(e *provider.NoKeyError) string {
	if e.EnvVar != "" {
		return e.Error() + ". /provider edit " + e.Provider + " pastes the key instead, or set " + e.EnvVar + " and start " + app.Name + " again."
	}
	return e.Error() + ". /provider edit " + e.Provider + " adds one."
}

// kTokens writes a token count briefly: 1M, 200k, 64k.
func kTokens(n int) string {
	switch {
	case n >= 1000000 && n%1000000 == 0:
		return fmt.Sprintf("%dM", n/1000000)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}
