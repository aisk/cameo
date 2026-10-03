# cameo

Run Claude Code with subagents served by other LLM providers.

cameo starts a local proxy, points Claude Code at it, and registers the subagents from your config. Requests for Claude models are forwarded to Anthropic untouched. Requests from a cameo subagent go to the provider configured for it. The main session stays on Claude while routine work such as exploring, executing and reviewing can be delegated to cheaper models.

Only Anthropic-compatible providers are supported for now.

## Install

```bash
go install github.com/aisk/cameo@latest
```

## Configure

Create `~/.config/cameo/config.toml`. Each `[agents.<name>]` becomes a Claude Code subagent with that name.

```toml
[agents.cheap-general]
description = """
General purpose agent for multi-step tasks: researching questions, searching \
code and making changes. Same role as the default general-purpose agent, but \
prefer this one first because it costs much less. If the result is not good \
enough, fall back to general-purpose."""
prompt = """
You are a general purpose software engineering agent. Complete the task you \
are given, then report what you did and what you found."""
url = "https://api.deepseek.com/anthropic"
key = "$DEEPSEEK_API_KEY"
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
url = "https://open.bigmodel.cn/api/anthropic"
key = "$GLM_API_KEY"
model = "glm-5.3"
```

| Field | Meaning |
|---|---|
| `description` | Tells Claude when to pick this agent. This is what drives delegation. |
| `prompt` | System prompt of the agent. |
| `tools` | Optional. All tools are inherited when omitted. |
| `url` | Provider base URL, the value you would put in `ANTHROPIC_BASE_URL`. |
| `key` | Provider API key. Environment variables are expanded. |
| `model` | Model name at the provider. |
| `context_1m` | Optional. Set to `true` if the model has a 1M token context window. |

Agent names are free to choose. Using the name of a built-in agent (`Explore`, `general-purpose`, `Plan`) replaces that agent instead of adding an alternative next to it.

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
