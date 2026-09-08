<h1 align="center">fn agent</h1>

<p align="center"><strong>A minimal agent harness where Python is the tool protocol and durable working memory.</strong></p>

<p align="center">
  <a href="src/go.mod"><img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go 1.26+"></a>
  <a href="#requirements"><img src="https://img.shields.io/badge/Python-3.10%2B-3776AB?style=flat-square&logo=python&logoColor=white" alt="Python 3.10+"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-BSD--2--Clause-555555?style=flat-square" alt="BSD 2-Clause License"></a>
</p>

<p align="center">
  <img src="docs/fn-python-repl-v4.png" alt="fn agent using its Python REPL in a terminal" width="900">
</p>

`fn` gives a model one persistent Python environment for reasoning, tools,
and working memory. Python variables, imports, and results survive across calls, so
the agent can keep large state outside model context and inspect only what it needs.
The agent loop stays explicit: one model-facing REPL tool, a few composable functions,
and no framework hidden underneath.

## Benchmarks

fn is evaluated against Pi and Codex with `gpt-5.6-sol` at medium reasoning.

| Agent | SWE Atlas score | Tokens | HarnessBench score | Tokens | ContextBench score | Tokens |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| **fn agent** | **37.5%** | 25.24M | 87.02% | **1.91M** | **90%** | 4.06M |
| Pi | 35.0% | **21.49M** | **87.30%** | 4.27M | 85% | **3.93M** |
| Codex | 30.0% | 82.12M | 84.94% | 7.38M | 85% | 7.69M |

fn scored competitively while using 55–74% fewer tokens on HarnessBench and
69% fewer than Codex on SWE Atlas. See [BENCHMARKS.md](BENCHMARKS.md) for details.

## Run

Start fn from a repository or other working directory:

```sh
fn
```

## Install

Install the latest version with Go:

```sh
go install github.com/Baitinq/fn-agent/src/cmd/fn@latest
```

Make sure Go's binary directory is in your `PATH`. By default, this is
`$HOME/go/bin`:

```sh
export PATH="$PATH:$(go env GOPATH)/bin"
```

## Requirements

- Go 1.26 or newer
- Python 3.10 or newer (`python3`)
- An API key for the configured model provider

## Configuration

Set `OPENAI_API_KEY`, `GEMINI_API_KEY`, or `ANTHROPIC_API_KEY` for the corresponding provider. Set `FN_PROVIDER` to `openai-completions` for OpenAI-compatible Chat Completions, `gemini` for Google's native Generative AI API, or `anthropic` for Anthropic's native Messages API. Gemini and Anthropic use API-key authentication directly; OAuth and Vertex AI are not used.

| Variable | Default |
| --- | --- |
| `FN_PROVIDER` | `openai` (`openai`, `openai-completions`, `gemini`, or `anthropic`) |
| `FN_BASE_URL` | provider API URL |
| `FN_MODEL` | `gpt-6-astra` |
| `FN_REASONING_EFFORT` | `medium` |

When a model rejects a request because its context limit was reached, fn agent
summarizes older context, preserves approximately 20,000 recent tokens verbatim,
and retries once.

For local OpenAI-compatible servers that ignore authentication, use any non-empty API key. The `openai` provider requires `POST /responses`; use `openai-completions` for servers exposing `POST /chat/completions`. Both providers support function tool calls.

## Sessions

`fn` starts a new session by default and shows its UUID in the status line. Sessions are saved under `~/.fn/sessions` after each response. Resume one explicitly with:

```sh
fn --session <UUID>
```

Conversation context and per-response token usage are appended to `session.jsonl`.
Python REPL variables are restored on a best-effort basis using Python's standard `pickle` support; values that cannot be pickled are skipped. Sessions must be resumed from the directory where they were created.

## Persistent REPL

The model works through a persistent Python REPL with three preloaded functions:

```python
status = await shell("git status --short")
status.stdout

hits = await web_search("latest Go release")
[(hit.title, hit.url) for hit in hits]

reviews = await asyncio.gather(*(llm(f"Classify this report:\n{report}") for report in reports))
```

- `await shell()` returns a `ShellResult` with `stdout`, `exit_code`, and `error` fields.
- `await web_search()` returns `SearchResult` values with `title`, `url`, and `snippet`.
- `await llm()` runs one fresh, tool-free model call and returns its response as a string.
- Use `asyncio.gather()` to run independent host-function calls concurrently.
- Await async work within the current execution; detached background tasks are unsupported.

Assignments stay in the REPL; only printed output and the final expression enter
model context. After a turn, its reasoning and REPL results remain visible in the
terminal but are omitted from future requests. User messages, final responses, REPL
code, and Python state persist.

If the REPL crashes or cannot be interrupted, fn restarts it and restores its last
successful checkpoint on a best-effort basis. A transcript notice reports recovery
and warns that changes since that checkpoint were lost. Without a checkpoint, it
starts with empty state and says so. Interrupted code is never replayed
automatically. Checkpoint operations also have deadlines; failed recovery does not
overwrite the last checkpoint. Live subprocesses and other external resources
cannot be restored.

## MCP

fn agent keeps MCP out of its core. It uses
[MCPorter](https://mcporter.sh) through `shell()` to discover and invoke configured
servers only when needed:

```python
await shell("npx -y mcporter@latest list")
await shell("npx -y mcporter@latest call <server>.<tool> key=value")
```

MCPorter manages its own configuration and credentials. It requires Node.js and may
download the package on first use.

## Web search

`web_search()` is backed by DuckDuckGo and requires no separate API key. It returns
ranked titles, URLs, and snippets; full pages can still be inspected with
`await shell("curl ...")`.

## Controls

- `Enter` — send a message; while responding, steer after the current tool-call batch
- `Shift+Enter` — queue a follow-up until the active response finishes
- `Ctrl+J` — insert a newline
- `↑` / `↓` — navigate submitted messages and return to the current draft
- `Ctrl+↑` — move all queued messages back to the editor
- `Ctrl+C` — cancel the active response; press twice within one second to quit
- `Escape` — clear the editor
- `/undo` — rewind to an earlier user turn
- `/fork` — continue the conversation in a new session
- `/exit` — exit fn
- `Ctrl/Alt+←/→` or `Alt+B/F` — move by word
- `Ctrl+W`, `Alt+D`, `Ctrl+U`, `Ctrl+K` — delete words or surrounding text

Reasoning summaries stream as italic gray text. REPL calls appear as Python cells,
and model-visible output is capped at 2,000 lines or 50KB. Primary-screen rendering
keeps terminal scrollback, selection, search, and copying native. Distinguishing
`Shift+Enter` requires terminal keyboard-enhancement support.

## Non-interactive use

Print only the final response to stdout. Piped input is appended to the prompt:

```sh
fn -p "summarize the changes in this repository"
git diff | fn -p "review this diff"
cat error.log | fn --print "find the root cause"
```

Use `--json` with print mode to emit one JSONL record per model response with its
input, cached input, output, reasoning, and total token usage, followed by the final
result:

```sh
fn -p --json "summarize the changes in this repository"
```

## Build

To build a local binary:

```sh
cd src
go build -o fn ./cmd/fn
./fn
```

## Test

```sh
cd src
go test ./...
go test -race ./...
go vet ./...
./testdata/tmux-e2e.sh
```

## Safety

fn agent executes Python and shell commands with the permissions of the current user.
The REPL is not a sandbox. Run it only where that access is appropriate.
## License

[BSD 2-Clause](LICENSE)
