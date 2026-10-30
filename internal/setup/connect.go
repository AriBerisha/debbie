package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"debbie/internal/config"
	"debbie/internal/effort"
	"debbie/internal/provider"
)

// Conn is a provider ready for a session.
type Conn struct {
	Name          string
	Provider      config.Provider   // a copy, with the model filled in
	Adapter       provider.Provider // *provider.OpenAI or *provider.Anthropic
	APIKey        string
	ContextWindow int // 0 when unknown
	EffortLevels  []effort.Level
	Warnings      []string
	// AutoCompact is the context size at which the conversation is
	// compacted (Manager.AutoCompactFor); the program fills it in.
	AutoCompact int
}

// Connect prepares a provider for a session: it reads the API key, builds
// the adapter, and asks the server for what the config leaves out: the
// model, the context size, and the reasoning-effort levels.
func Connect(ctx context.Context, name string, p config.Provider, getenv func(string) string) (*Conn, error) {
	c := &Conn{Name: name, APIKey: p.APIKey}
	if p.APIKeyEnv != "" {
		c.APIKey = getenv(p.APIKeyEnv)
	}
	switch p.Adapter {
	case config.AdapterAnthropic:
		return connectClaude(ctx, c, p)
	case config.AdapterOpenAI:
	default:
		return nil, fmt.Errorf("provider %s uses the %s adapter, which this version doesn't know", name, p.Adapter)
	}
	if p.APIKeyEnv != "" && c.APIKey == "" {
		c.Warnings = append(c.Warnings, p.APIKeyEnv+" is not set; sending requests without an API key")
	}
	a, err := provider.NewOpenAI(name, &p, c.APIKey)
	if err != nil {
		return nil, err
	}
	c.ContextWindow = p.ContextWindow
	if preset, _ := FindPreset(p.Preset); p.Preset == "" || preset.Props {
		// llama-server reports its per-slot context and the reasoning-effort
		// levels of its chat template; other servers just fail this.
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if info, err := a.ServerInfo(sctx); err == nil {
			c.EffortLevels = info.EffortLevels
			a.SetEffortLevels(info.EffortLevels)
			if c.ContextWindow == 0 {
				c.ContextWindow = info.ContextWindow
			}
		}
		cancel()
	}
	if p.Model == "" || c.ContextWindow == 0 {
		mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		models, err := a.Models(mctx)
		cancel()
		if p.Model == "" {
			model, err := pickModel(models, err)
			if err != nil {
				return nil, err
			}
			p.Model = model
			a.SetModel(model)
		}
		for _, m := range models {
			if m.ID == p.Model && c.ContextWindow == 0 {
				c.ContextWindow = m.ContextWindow
			}
		}
	}
	c.Provider, c.Adapter = p, a
	return c, nil
}

// modelLister is an adapter that can list its server's models.
type modelLister interface {
	Models(ctx context.Context) ([]provider.ModelInfo, error)
}

// Models lists the models the provider's server offers, for /model.
func (c *Conn) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	a, ok := c.Adapter.(modelLister)
	if !ok {
		return nil, fmt.Errorf("provider %s can't list its models", c.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return a.Models(ctx)
}

// SetModel switches the connection to another of its server's models, for
// /model, and asks the server what the model supports: Claude's models API
// says its context, its output limit, whether it sees images and the effort
// levels it takes, and a model it doesn't know is an error. An
// OpenAI-compatible server's model list says the context when it reports
// one; otherwise the context stays as it was. A model such a server doesn't
// list is used anyway, with a warning, since not every server lists what
// it serves.
func (c *Conn) SetModel(ctx context.Context, model string) (warning string, err error) {
	switch a := c.Adapter.(type) {
	case *provider.OpenAI:
		mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		models, err := a.Models(mctx)
		cancel()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		listed := false
		for _, m := range models {
			if m.ID == model {
				listed = true
				if m.ContextWindow > 0 {
					c.ContextWindow = m.ContextWindow
				}
			}
		}
		if err == nil && len(models) > 0 && !listed {
			warning = fmt.Sprintf("%s doesn't list the model %s; using it anyway", c.Name, model)
		}
		a.SetModel(model)
		c.Provider.Model = model
	case *provider.Anthropic:
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		info, err := a.Describe(dctx, model)
		cancel()
		if err != nil {
			return "", err
		}
		a.SetModel(model)
		a.SetCapabilities(info)
		old := c.Provider
		fillClaude(&c.Provider, info, &old)
		c.ContextWindow = c.Provider.ContextWindow
		c.EffortLevels = a.EffortLevels()
	default:
		return "", fmt.Errorf("provider %s can't switch models", c.Name)
	}
	return warning, nil
}

// pickModel chooses the server's only model.
func pickModel(models []provider.ModelInfo, listErr error) (string, error) {
	if listErr != nil {
		return "", fmt.Errorf("can't list the server's models (%w); name one with --model", listErr)
	}
	switch len(models) {
	case 0:
		return "", errors.New("the server lists no models; name one with --model")
	case 1:
		return models[0].ID, nil
	}
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return "", fmt.Errorf("the server offers %d models; choose one with --model: %s", len(models), strings.Join(ids, ", "))
}
