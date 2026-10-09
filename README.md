# cameo

cameo, named after the guest appearance in film, lets models from other LLM providers play subagents in Claude Code. This way you can use subscriptions from several AI providers together, with a SOTA model like Claude directing cheaper models that take over the simple and well-defined work.

<img src="https://cdn1.faroutmagazine.co.uk/uploads/1/2020/04/Hitchcock.jpg" alt="Alfred Hitchcock, known for cameos in his own films" width="100%">

cameo starts a local proxy, points Claude Code at it, and registers the subagents from your config. Requests for Claude models are forwarded to Anthropic untouched. Requests from a cameo subagent go to the provider configured for it. The main session stays on Claude while routine work such as exploring, executing and reviewing can be delegated to cheaper models.

Providers can speak the Anthropic Messages API, OpenAI Chat Completions, OpenAI Responses or Gemini. For anything but Anthropic, cameo translates the requests and replies on the way, with [some limits](#translation-limits). A ChatGPT subscription can serve agents too, through the built-in [`chatgpt` provider](#chatgpt-subscription).

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
| `provider` | Name of the `[providers.<name>]` table that serves this agent, or a built-in provider such as `chatgpt`. |
| `model` | Model name at the provider. |
| `context_1m` | Optional. Set to `true` if the model has a 1M token context window. |

Agent names are free to choose. Using the name of a built-in agent (`Explore`, `general-purpose`, `Plan`) replaces that agent instead of adding an alternative next to it.

Provider names are free to choose too, except `chatgpt` and `antigravity`. They are reserved for built-in subscription providers, which have no table in the config. `chatgpt` is described below, `antigravity` is not supported yet.

Older configs had `url` and `key` on each agent. Move them into a provider table and reference it with `provider`.

## ChatGPT subscription

The built-in `chatgpt` provider serves agents through your ChatGPT plan instead of an API key. It uses OpenAI's Sign in with ChatGPT, which lets an app on your machine send requests to OpenAI's API on your plan. Sign in once:

```bash
cameo provider login chatgpt
```

This opens OpenAI's sign-in page in your browser and waits for it to come back on port 1455, or on another port if that one is taken. If cameo runs on another machine than the browser, the last page will not load. Copy its address from the browser and paste it into the terminal instead.

Then name the provider in an agent. There is no `[providers.chatgpt]` table to write:

```toml
[agents.reviewer]
description = "Reviews a diff for bugs and unclear code."
prompt = "You review code changes. Never modify anything."
provider = "chatgpt"
model = "gpt-5.5"
```

`cameo provider models chatgpt` shows the models OpenAI lists for your account. The sign-in may also run newer models that the list leaves out.

Things to know:

- Whether an account can be used this way depends on its plan. If it cannot, the sign-in fails and says so.
- Usage counts against the limit of the plan, which is shared with ChatGPT itself and every other app the plan is used in. How much is left is shown at https://chatgpt.com/settings/usage.
- The sign-in is stored in `auth.json` next to the default config file, for example `~/.config/cameo/auth.json`, readable by you only. cameo renews it by itself. Next to it, `chatgpt-host` holds an id cameo makes up for this machine and tells OpenAI when signing in.
- `cameo provider logout chatgpt` removes the sign-in and asks OpenAI to revoke it.
- cameo will not start while an agent uses `chatgpt` and nobody is signed in.

## Run

```bash
cameo            # same as running claude
cameo -p "..."   # all arguments are passed to claude
```

The one exception is `cameo provider`, which cameo handles itself:

| Command | Meaning |
|---|---|
| `cameo provider login <name>` | Sign in to a built-in provider. Only `chatgpt` for now. |
| `cameo provider logout <name>` | Remove the saved sign-in and revoke it at the provider. |
| `cameo provider list` | Show the built-in providers and the ones from the config, with who is signed in. |
| `cameo provider models <name>` | Show the models the signed-in account can use. Only for `chatgpt`. |

| Variable | Meaning |
|---|---|
| `CAMEO_CONFIG` | Path of the config file. |
| `CAMEO_AUTH` | Path of the file the sign-ins are stored in. |
| `CAMEO_LOG` | Write the proxy log to this file. Off by default. |
| `ANTHROPIC_BASE_URL` | If already set, used as the upstream for Claude models. |

## Translation limits

Agents on an `anthropic` provider are forwarded as they are. For `chat`, `responses` and `gemini` providers the request is rebuilt in the other protocol, and a few things do not survive that:

- Prompt cache breakpoints (`cache_control`) are not forwarded. Whatever caching happens is the provider's own automatic caching.
- Anthropic server tools are dropped, since the provider cannot run them. Web search is the one exception, and only where the provider has its own: the built-in web search on `responses`, Google Search on `gemini` when the request offers no other tools, and the web plugin of OpenRouter on `chat`. On any other `chat` provider web search is dropped as well.
- Token counting (`/v1/messages/count_tokens`) is answered by cameo with a rough estimate from the size of the request, not a real count.

The `chatgpt` provider is stricter than a plain `responses` one, because OpenAI accepts less from a ChatGPT sign-in than from an API key. `max_tokens`, `temperature` and `top_p` are not forwarded.

## Acknowledgements

The protocol translation lives in the [`llmconv`](llmconv) package, a fork of the gateway code of [magpie](https://github.com/yetone/magpie) with changes to fit cameo. The `chatgpt` provider is ported from magpie as well.
