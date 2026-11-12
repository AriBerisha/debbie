package setup

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"debbie/internal/agent"
	"debbie/internal/config"
	"debbie/internal/effort"
	"debbie/internal/prompt"
)

// cloudParallel is how many agents run at once on a cloud provider, which
// takes many requests side by side, unless the config allows more. A server
// of the user's has as many parallel slots as it was started with, each with
// its share of the context, so there subagents.max_parallel decides.
const cloudParallel = 4

// Workers works out what the task tool's agents run on, from the config's
// subagents section as it is now: a provider and model of their own, such
// as a local server under a Claude lead, or else the conversation's.
type Workers struct {
	cfg    *config.Config
	getenv func(string) string
	secret func(...string)
	active func() *Conn // the provider of the conversation

	mu   sync.Mutex
	conn *Conn // the connection of the agents' own provider
	key  string
}

// NewWorkers returns what works out the agents' provider. secret learns the
// API keys it connects with, to keep them out of all output.
func NewWorkers(cfg *config.Config, getenv func(string) string, secret func(...string), active func() *Conn) *Workers {
	return &Workers{cfg: cfg, getenv: getenv, secret: secret, active: active}
}

// Current says what agents run on now, connecting to their provider the
// first time it is needed.
func (w *Workers) Current() (agent.Workers, error) {
	sc := w.cfg.Subagents
	level, _ := effort.Parse(sc.Effort)
	out := agent.Workers{Effort: level}
	if sc.Provider == "" && sc.Model == "" {
		out.Parallel = parallelFor(w.active().Provider.Preset, sc.MaxParallel)
		return out, nil
	}
	conn, err := w.connect(sc)
	if err != nil {
		return agent.Workers{}, err
	}
	out.Provider, out.MaxTokens = conn.Adapter, conn.Provider.MaxOutputTokens
	out.ContextWindow = conn.ContextWindow
	out.Vision = conn.Provider.Vision
	out.CompactAt = agent.AutoCompactAt(agent.DefaultAutoCompact, conn.ContextWindow)
	out.Parallel = parallelFor(conn.Provider.Preset, sc.MaxParallel)
	return out, nil
}

// Note tells the model what its agents run on, from the config alone:
// connecting just to say so would make the first message wait.
func (w *Workers) Note() string {
	sc := w.cfg.Subagents
	active := w.active()
	name, p := active.Name, active.Provider
	if q := w.cfg.Providers[sc.Provider]; q != nil {
		name, p = sc.Provider, *q
	}
	model := p.Model
	if sc.Model != "" {
		model = sc.Model
	}
	on := name
	switch {
	case sc.Provider == "" && sc.Model == "":
		on = "this conversation's own model"
	case model != "":
		on += " · " + model
	}
	return prompt.AgentsNote(on, parallelFor(p.Preset, sc.MaxParallel), p.Vision)
}

func (w *Workers) connect(sc config.Subagents) (*Conn, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	name, model := sc.Provider, sc.Model
	var p config.Provider
	switch {
	case name == "":
		active := w.active()
		name, p = active.Name, active.Provider
	case w.cfg.Providers[name] == nil:
		return nil, fmt.Errorf("subagents.provider names %q, which isn't a configured provider", name)
	default:
		p = *w.cfg.Providers[name]
	}
	if model != "" {
		p.Model = model
	}
	if key := name + "\x00" + p.Model; w.conn == nil || w.key != key {
		conn, err := Connect(context.Background(), name, p, w.getenv)
		if err != nil {
			return nil, err
		}
		w.secret(conn.APIKey)
		w.conn, w.key = conn, key
	}
	return w.conn, nil
}

// parallelFor is how many agents run at once on a provider of a preset.
func parallelFor(preset string, configured int) int {
	if p, ok := FindPreset(preset); ok && p.Cloud {
		return max(configured, cloudParallel)
	}
	return max(configured, 1)
}

// SetWorkers saves what the agents run on: a provider, or "" for the
// conversation's; a model, or "" for that provider's own; and how hard they
// reason, or effort.Default for the conversation's at most medium.
func (m *Manager) SetWorkers(name, model string, level effort.Level) error {
	if name != "" {
		if err := m.exists(name); err != nil {
			return err
		}
	}
	if level == effort.UltraWorkflow {
		return errors.New("ultraworkflow is for the conversation, not for its agents")
	}
	return m.change(func(c *config.Config) {
		c.Subagents.Provider, c.Subagents.Model, c.Subagents.Effort = name, model, string(level)
	})
}
