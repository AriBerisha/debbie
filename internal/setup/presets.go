// Package setup configures providers: the setup wizard, its probes, trust
// on first use for certificates, and the /provider commands (SPEC §6).
package setup

import "debbie/internal/config"

// Preset pre-fills the wizard for one kind of server (SPEC §4.1). The URL
// is only a suggestion.
type Preset struct {
	ID      string // stored as the provider's preset
	Name    string // shown in the wizard
	About   string // a few words about it, shown beside the name
	Adapter string
	URL     string // suggested base URL; "" when there is none
	KeyEnv  string // suggested environment variable for the API key
	Cloud   bool   // a cloud service, which needs an API key
	// KeysURL is the page of a cloud service's console where API keys are
	// made; KeyPrefix is what its keys start with, "" when unknown.
	KeysURL, KeyPrefix string
	Health             bool   // serves GET /health at the server root
	Props              bool   // llama-server's /props reports the context size
	ToolFix            string // the likely fix when tool calling doesn't work
}

// Presets lists the kinds of servers the wizard offers, self-hosted first.
var Presets = []Preset{
	{ID: "llama-server", Name: "llama-server", About: "llama.cpp's server, here or on your network", Adapter: config.AdapterOpenAI, URL: "http://localhost:8080/v1",
		KeyEnv: "LLAMA_API_KEY", Health: true, Props: true,
		ToolFix: "start llama-server with --jinja, so it uses the model's chat template, which handles tool calls"},
	{ID: "vllm", Name: "vLLM", About: "a vLLM server", Adapter: config.AdapterOpenAI, URL: "http://localhost:8000/v1",
		KeyEnv: "VLLM_API_KEY", Health: true,
		ToolFix: "start vLLM with --enable-auto-tool-choice and the --tool-call-parser for the model, such as hermes for Qwen2.5 or qwen3_xml for Qwen3-Coder"},
	{ID: "ollama", Name: "Ollama", About: "models Ollama runs", Adapter: config.AdapterOpenAI, URL: "http://localhost:11434/v1",
		KeyEnv: "OLLAMA_API_KEY", ToolFix: "use a model whose Ollama template supports tools"},
	{ID: "lmstudio", Name: "LM Studio", About: "LM Studio's local server", Adapter: config.AdapterOpenAI, URL: "http://localhost:1234/v1",
		KeyEnv: "LMSTUDIO_API_KEY", ToolFix: "load a model that supports tool use"},
	{ID: "openai-compatible", Name: "Other OpenAI-compatible server", About: "any server with OpenAI's chat API", Adapter: config.AdapterOpenAI,
		KeyEnv: "OPENAI_COMPATIBLE_API_KEY", ToolFix: "check that the server supports the tools parameter of /chat/completions"},
	{ID: "claude", Name: "Claude (Anthropic)", About: "Anthropic's models, with an API key", Adapter: config.AdapterAnthropic, URL: "https://api.anthropic.com",
		KeyEnv: "ANTHROPIC_API_KEY", Cloud: true, KeysURL: claudeKeysURL, KeyPrefix: "sk-ant-"},
	// OpenAI's own API is spoken to as the Responses API (SPEC §4.6): its
	// newest models take function tools with reasoning only there.
	{ID: "openai", Name: "OpenAI", About: "OpenAI's models, with an API key", Adapter: config.AdapterOpenAIResponses, URL: "https://api.openai.com/v1",
		KeyEnv: "OPENAI_API_KEY", Cloud: true, KeysURL: "https://platform.openai.com/api-keys", KeyPrefix: "sk-"},
	{ID: "gemini", Name: "Gemini", About: "Google's models, with an API key", Adapter: config.AdapterOpenAI, URL: "https://generativelanguage.googleapis.com/v1beta/openai",
		KeyEnv: "GEMINI_API_KEY", Cloud: true, KeysURL: "https://aistudio.google.com/apikey", KeyPrefix: "AIza"},
	// Kimi's API follows OpenAI's chat API (platform.kimi.ai/docs, read
	// 2026-10-30): the K3 models, kimi-k3.1-pro among them, with tool calls.
	{ID: "kimi", Name: "Kimi (Moonshot AI)", About: "Moonshot's Kimi models, with an API key", Adapter: config.AdapterOpenAI, URL: "https://api.moonshot.ai/v1",
		KeyEnv: "MOONSHOT_API_KEY", Cloud: true, KeysURL: "https://platform.kimi.ai/console/api-keys", KeyPrefix: "sk-"},
}

// FindPreset returns the preset with the given ID. An unknown or empty ID
// gives the generic OpenAI-compatible preset, with ok false.
func FindPreset(id string) (Preset, bool) {
	for _, p := range Presets {
		if p.ID == id {
			return p, true
		}
	}
	for _, p := range Presets {
		if p.ID == "openai-compatible" {
			return p, false
		}
	}
	panic("no openai-compatible preset")
}
