# cameo

cameo, named after the guest appearance in film, lets models from other LLM providers play subagents in Claude Code. This way you can use subscriptions from several AI providers together, with a SOTA model like Claude directing cheaper models that take over the simple and well-defined work.

<img src="https://cdn1.faroutmagazine.co.uk/uploads/1/2020/04/Hitchcock.jpg" alt="Alfred Hitchcock, known for cameos in his own films" width="100%">

cameo starts a local proxy, points Claude Code at it, and registers the subagents from your config. Requests for Claude models are forwarded to Anthropic untouched. Requests from a cameo subagent go to the provider configured for it. The main session stays on Claude while routine work such as exploring, executing and reviewing can be delegated to cheaper models.

Providers can speak the Anthropic Messages API, OpenAI Chat Completions, OpenAI Responses or Gemini. For anything but Anthropic, cameo translates the requests and replies on the way, with [some limits](#translation-limits).

## Install

```bash
go install github.com/aisk/cameo@latest
```

## Configure

Create `~/.config/cameo/config.toml`. Each `[providers.<name>]` is one credential at one endpoint, and each `[agents.<name>]` becomes a Claude Code subagent with that name, served by the provider it names. Several agents can share a provider.

```toml
[providers.deepseek]
url = "https://api.deepseek.com/anthropic"
key = "$DEEPSEEK_API_KEY"

[providers.glm]
url = "https://open.bigmodel.cn/api/anthropic"
key = "$GLM_API_KEY"

[providers.openai]
api = "responses"
url = "https://api.openai.com/v1"
key = "$OPENAI_API_KEY"

[agents.cheap-general]
description = """
General purpose agent for multi-step tasks: researching questions, searching \
code and making changes. Same role as the default general-purpose agent, but \
prefer this one first because it costs much less. If the result is not good \
enough, fall back to general-purpose."""
prompt = """
You are a general purpose software engineering agent. Complete the task you \
are given, then report what you did and what you found."""
provider = "deepseek"
model = "deepseek-flash"

[agents.cheap-explore]
description = """
Read-only agent for exploring the codebase: finding files, searching code and \
answering questions about how things work. Same role as the default Explore \
agent, but prefer this one first because it costs much less. If the result is \
not good enough, fall back to Explore."""
prompt = """
You are a read-only codebase exploration agent. Find what you are asked for \
and report it with file paths and line numbers. Never modify anything."""
tools = ["Read", "Grep", "Glob"]
provider = "glm"
model = "glm-5.3"

[agents.branch-cleaner]
description = """
Cleans up local git branches of the current project. Use it whenever the user \
asks to tidy up, prune or delete stale local branches."""
prompt = """
You clean up local git branches. Run `git fetch --prune`, then delete local \
branches that are already merged into the main branch, using `git branch -d` \
only. Never use `-D`, never delete the current branch or the main branch, and \
never touch remote branches. Report which branches you deleted and which ones \
you kept and why."""
tools = ["Bash"]
provider = "deepseek"
model = "deepseek-flash"

[agents.cheap-reviewer]
description = """
Reviews a diff for bugs and unclear code. Use it for a second opinion on \
small and medium changes."""
prompt = """
You review code changes. Report concrete problems with file paths and line \
numbers, most serious first. Never modify anything."""
tools = ["Read", "Grep", "Glob", "Bash"]
provider = "openai"
model = "gpt-5-mini"
```

Provider fields:

| Field | Meaning |
|---|---|
| `api` | Optional. The protocol the provider speaks: `anthropic` (default), `chat`, `responses` or `gemini`. |
| `url` | Provider base URL. cameo appends the endpoint, see the table below. |
| `key` | Provider API key. Environment variables are expanded. |

| `api` | Protocol | Example `url` | Requests go to |
|---|---|---|---|
| `anthropic` | Anthropic Messages | `https://api.deepseek.com/anthropic` | `<url>/v1/messages`. This is the value you would put in `ANTHROPIC_BASE_URL`. |
| `chat` | OpenAI Chat Completions | `https://api.openai.com/v1` | `<url>/chat/completions` |
| `responses` | OpenAI Responses | `https://api.openai.com/v1` | `<url>/responses` |
| `gemini` | Google Gemini | `https://generativelanguage.googleapis.com/v1beta` | `<url>/models/<model>:streamGenerateContent?alt=sse` |

Agent fields:

| Field | Meaning |
|---|---|
| `description` | Tells Claude when to pick this agent. This is what drives delegation. |
| `prompt` | System prompt of the agent. |
| `tools` | Optional. All tools are inherited when omitted. |
| `provider` | Name of the `[providers.<name>]` table that serves this agent. |
| `model` | Model name at the provider. |
| `context_1m` | Optional. Set to `true` if the model has a 1M token context window. |

Agent names are free to choose. Using the name of a built-in agent (`Explore`, `general-purpose`, `Plan`) replaces that agent instead of adding an alternative next to it.

Provider names are free to choose too, except `codex` and `antigravity`, which are reserved for subscription providers that are not supported yet.

Older configs had `url` and `key` on each agent. Move them into a provider table and reference it with `provider`.

## Run

```bash
cameo            # same as running claude
cameo -p "..."   # all arguments are passed to claude
```

| Variable | Meaning |
|---|---|
| `CAMEO_CONFIG` | Path of the config file. |
| `CAMEO_LOG` | Write the proxy log to this file. Off by default. |
| `ANTHROPIC_BASE_URL` | If already set, used as the upstream for Claude models. |

## Translation limits

Agents on an `anthropic` provider are forwarded as they are. For `chat`, `responses` and `gemini` providers the request is rebuilt in the other protocol, and a few things do not survive that:

- Prompt cache breakpoints (`cache_control`) are not forwarded. Whatever caching happens is the provider's own automatic caching.
- Anthropic server tools are dropped, since the provider cannot run them. Web search is the one exception, and only where the provider has its own: the built-in web search on `responses`, Google Search on `gemini` when the request offers no other tools, and the web plugin of OpenRouter on `chat`. On any other `chat` provider web search is dropped as well.
- Token counting (`/v1/messages/count_tokens`) is answered by cameo with a rough estimate from the size of the request, not a real count.

## Acknowledgements

The protocol translation lives in the [`llmconv`](llmconv) package, a fork of the gateway code of [magpie](https://github.com/yetone/magpie) with changes to fit cameo.
