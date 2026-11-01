# debbie

A terminal coding agent for the models you run. One static binary, written in Go with the
standard library and nothing else. It talks to the model endpoint you configure and to
nothing else: no telemetry, no update checks, no accounts.

- Self-hosted first: llama-server, vLLM, Ollama, LM Studio, any OpenAI-compatible URL.
- Cloud when you want it: Claude, OpenAI, Gemini, Kimi, with your own API key.
- Windows, macOS and Linux.

## Install

Windows (PowerShell):

```powershell
irm https://debbie.sh/install.ps1 | iex
```

macOS and Linux:

```sh
curl -fsSL https://debbie.sh/install.sh | sh
```

Or build it yourself with Go 1.27 or newer:

```sh
go build ./cmd/debbie
./debbie install    # puts debbie on your PATH; "debbie uninstall" undoes it
```

Then run `debbie` in a project folder. The first start asks which model provider you use,
checks the connection and saves it. Everything is kept in one JSON file under your user
profile; `debbie` never writes anywhere else, apart from the project it works in.

## Using it

Type what you want done. The agent reads and searches the project, edits files and runs
commands, asking before each edit and command unless you choose another mode. A few
commands:

| Command | What it does |
|---|---|
| `/provider` | switch between your providers, or add one |
| `/model` | switch the model of the current provider |
| `/mode` | plan, default, editor, auto or yolo: how much runs without asking |
| `/effort` | how hard the model reasons, on a slider |
| `/budget` | cap how long the model thinks per reply, for llama-server |
| `/autocompact` | when a long conversation is summarized to free context, on a slider |
| `/resume` | go back to an earlier conversation in this folder (`debbie -c` resumes the latest) |
| `/cost` | what the session cost, per model; set your own prices per million tokens |
| `/web` | web access for the agent: off (the default), safe or yolo |
| `/help` | the rest |

## Web access and the Chrome extension

Web access is off until you turn it on. With it on, the agent can fetch pages and, through
a companion Chrome extension, search and browse in your own Chrome: only on the sites you
allow (when the agent needs another, a window in Chrome asks you whether to allow it), in a
tab of its own, never typing into password or payment fields, and asking before anything
that buys, posts, signs in or deletes. The extension ships inside the binary, makes no
network requests of its own and never updates itself. `debbie browser install` sets it up.

## Privacy

`debbie` opens connections only to the model endpoint you configured, to the web pages you let
it fetch, and to the MCP servers you list. Keys stay in your config file, readable only by
your account, and are stripped from the environment of every command it runs. Pages and
search results are handled as untrusted data.

Commands you approve run as you and can use the network like any program you start. If you
want a hard guarantee that nothing but the model server is ever reached, restrict the
machine's outbound traffic to that server; that only fits a setup with web access off.

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). Keep the attribution when
you redistribute it. Parts of the prompts and a few routines are derived from Qwen Code and
Gemini CLI (Apache 2.0); [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) lists them.
