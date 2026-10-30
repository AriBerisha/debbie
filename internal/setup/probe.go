package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"debbie/internal/config"
	"debbie/internal/effort"
	"debbie/internal/provider"
)

// prober runs the checks of SPEC §6 against one provider. Every request
// goes through the provider adapter to the provider's own endpoint.
type prober struct {
	name   string // for error messages
	prov   *config.Provider
	key    string
	preset Preset
}

func (pr *prober) adapter(p *config.Provider) (*provider.OpenAI, error) {
	return provider.NewOpenAI(pr.name, p, pr.key)
}

// health asks llama-server or vLLM whether they are up. ok is false, with
// no error, when the server has no /health and the models request has to
// tell.
func (pr *prober) health(ctx context.Context) (ok bool, err error) {
	if !pr.preset.Health {
		return false, nil
	}
	a, err := pr.adapter(pr.prov)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = a.Health(ctx)
	var api *provider.APIError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &api) && api.Status == http.StatusNotFound:
		return false, nil
	case errors.As(err, &api) && api.Status == http.StatusServiceUnavailable:
		return false, errors.New("the server is still loading its model (/health answered 503); try again when it's ready")
	}
	return false, err
}

// models lists the server's models. A 404 for a base URL without a path
// usually means the API lives under /v1; alt is then that URL, if it works.
func (pr *prober) models(ctx context.Context) (models []provider.ModelInfo, alt string, err error) {
	a, err := pr.adapter(pr.prov)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	models, err = a.Models(ctx)
	var api *provider.APIError
	if err == nil || !errors.As(err, &api) || api.Status != http.StatusNotFound {
		return models, "", err
	}
	if u, perr := url.Parse(pr.prov.BaseURL); perr != nil || strings.Trim(u.Path, "/") != "" {
		return nil, "", err
	}
	p := *pr.prov
	p.BaseURL = strings.TrimRight(p.BaseURL, "/") + "/v1"
	b, berr := pr.adapter(&p)
	if berr != nil {
		return nil, "", err
	}
	if m, err2 := b.Models(ctx); err2 == nil {
		return m, p.BaseURL, nil
	}
	return nil, "", err
}

// serverInfo reads llama-server's /props.
func (pr *prober) serverInfo(ctx context.Context) (provider.ServerInfo, error) {
	a, err := pr.adapter(pr.prov)
	if err != nil {
		return provider.ServerInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return a.ServerInfo(ctx)
}

var hello = []provider.Message{provider.TextMessage(provider.RoleUser, "Say OK.")}

// streamUsage reports whether streamed replies can carry token usage: a
// one-token request with stream_options, and one without if the server
// rejects that.
func (pr *prober) streamUsage(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	try := func(usage bool) error {
		p := *pr.prov
		p.Options.StreamUsage = usage
		a, err := pr.adapter(&p)
		if err != nil {
			return err
		}
		stream, err := a.Stream(ctx, provider.Request{Messages: hello, MaxTokens: 1})
		if err != nil {
			return err
		}
		defer stream.Close()
		for {
			if _, err := stream.Recv(); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
		}
	}
	err := try(true)
	if err == nil {
		return true, nil
	}
	if !clientError(err) {
		return false, err
	}
	if err := try(false); err != nil {
		return false, err
	}
	return false, nil
}

// clientError reports whether the server refused a request as malformed,
// as opposed to failing, or refusing the API key.
func clientError(err error) bool {
	var api *provider.APIError
	return errors.As(err, &api) && api.Status >= 400 && api.Status < 500 &&
		api.Status != http.StatusUnauthorized && api.Status != http.StatusForbidden
}

// getTime is the tool the tool-calling check offers.
var getTime = provider.ToolSpec{
	Name:        "get_time",
	Description: "Returns the current date and time.",
	Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
}

// toolCall checks that the model calls a tool when it has to: one request
// offering get_time, with a question only that tool can answer. levels are
// the chat template's reasoning-effort levels; when there are any, the
// request turns reasoning off, which makes the check quick.
func (pr *prober) toolCall(ctx context.Context, levels []effort.Level) (ok bool, problem string, err error) {
	a, err := pr.adapter(pr.prov)
	if err != nil {
		return false, "", err
	}
	req := provider.Request{
		Messages:  []provider.Message{provider.TextMessage(provider.RoleUser, "What time is it right now? Use the get_time tool to find out.")},
		Tools:     []provider.ToolSpec{getTime},
		MaxTokens: 1024,
	}
	if len(levels) > 0 {
		a.SetEffortLevels(levels)
		req.Effort = effort.None
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	stream, err := a.Stream(ctx, req)
	if clientError(err) {
		return false, "the server refused the tools parameter: " + err.Error(), nil
	}
	if err != nil {
		return false, "", err
	}
	defer stream.Close()
	stop := provider.StopEnd
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
			stop = e.StopReason
		}
	}
	if stop == provider.StopMaxTokens {
		return false, "the reply reached the output limit before it called the tool", nil
	}
	return false, "the model answered in plain text instead of calling the tool", nil
}
