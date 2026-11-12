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
| `/effort` | how hard the model reasons; `/effort budget` caps its thinking per reply, for llama-server |
| `/compact` | summarize the conversation to free context; `/compact auto` sets when it happens by itself |
| `/resume` | go back to an earlier conversation in this folder (`debbie -c` resumes the latest); `/resume rename` names this one |
| `/cost` | what the session cost, per model; set your own prices per million tokens |
| `/web` | web access for the agent: off (the default), safe or yolo; `/web browser` for Chrome's connection |
| `/help` | the rest; `/help tools` lists the tools |

## Agents

On any provider, the model can start agents when a job splits into independent parts: several
side by side, or in the background while it keeps working, in which case the agent's report
reaches it by itself. In the fullscreen UI, ↓ from an empty input box shows them: Enter opens
one, Delete or Ctrl+C in its view stops it. `/effort ultraworkflow` pushes the model to split
substantial work; it reasons at high effort then.

Agents can run on something cheaper than the conversation. `/provider workers` says what they
run on, and changes it:

```text
/provider workers home                     a Claude lead with workers on your local server
/provider workers claude claude-haiku-5-5  the same provider, another model
/provider workers home qwen3.8-flash low   with a model and an effort of their own
/provider workers off                      back to the conversation's
```

Unless you set an effort for them, agents reason at the conversation's, at most medium. Up to
four run at once on a cloud provider; on a server of your own, `subagents.max_parallel` decides
(1 by default, since each llama-server slot gets its share of the context). What they spend
shows in `/cost`, per model.

The model knows what its agents run on, how many run at a time, and whether they see images, so
it can keep work that takes looking at a picture for itself. `read_file` shows a model PNG, JPEG,
GIF and WebP images if its provider has `vision` on, as Claude does; `@shot.png` asks it to.

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
