# AI CLI Proxy — Go/Gin Edition

A high-performance Go port of the AI API proxy, built with [Gin](https://github.com/gin-gonic/gin).
It exposes a single server that speaks OpenAI, Anthropic Claude, Google Gemini, and Ollama wire
protocols, routing every request to whichever upstream provider pool is configured.

---

## What this is

`ai-cli-proxy-go` is a Go/Gin rewrite of the original Node.js proxy.  It presents one unified
HTTPS endpoint to downstream clients (LLM IDEs, code assistants, scripts) while distributing load
across multiple provider credentials via a pooled, health-aware round-robin selector.

Key capabilities:

- **Multi-protocol surface** — OpenAI, Claude, Gemini (v1beta), and Ollama APIs all work on the
  same port.  Clients need only change the `base_url`.
- **Provider pools** — Multiple API keys / OAuth credential files per provider.  Unhealthy entries
  are detected and automatically retried on a schedule.
- **Streaming** — SSE streaming for OpenAI and Claude; newline-delimited JSON for Ollama.
- **System prompt injection** — A configurable system prompt is prepended to every conversation.
- **Token counting** — `POST /v1/messages/count_tokens` returns a fast byte-based estimate.

---

## Build and run

### Prerequisites

- Go 1.22 or later
- `configs/config.json` (copy from `configs/config.json.example`) or supply everything via CLI flags

### Build

```sh
make build          # produces bin/ai-proxy
```

### Run

```sh
make run            # go run main.go (reads configs/config.json)
make run-dev        # go run main.go --port 3000 --api-key sk-1234567890
```

Or run the compiled binary directly:

```sh
./bin/ai-proxy --port 8080 --api-key my-secret-key
```

### Other targets

```sh
make test   # go test ./...
make tidy   # go mod tidy && go mod download
make clean  # remove bin/
```

---

## Configuration

Configuration is layered: `configs/config.json` is loaded first, then CLI flags override individual
fields, and finally certain environment variables are respected.

### `configs/config.json` (selected fields)

| Field                        | Default               | Description                                              |
|------------------------------|-----------------------|----------------------------------------------------------|
| `REQUIRED_API_KEY`           | `""`                  | Bearer token clients must supply.  Empty = auth disabled.|
| `SERVER_PORT`                | `3000`                | TCP port to listen on.                                   |
| `HOST`                       | `"0.0.0.0"`           | Bind address.                                            |
| `MODEL_PROVIDER`             | `"gemini-cli-oauth"`  | Default provider when none is inferred from the model.   |
| `PROJECT_ID`                 | `""`                  | Google Cloud project ID (Gemini CLI OAuth).              |
| `GEMINI_OAUTH_CREDS_FILE_PATH` | `""`               | Path to `application_default_credentials.json`.          |
| `GEMINI_OAUTH_CREDS_BASE64`  | `""`                  | Base64-encoded credentials (alternative to file path).   |
| `OPENAI_API_KEY`             | `""`                  | OpenAI API key.                                          |
| `OPENAI_BASE_URL`            | `"https://api.openai.com/v1"` | Override for custom OpenAI-compatible endpoints. |
| `CLAUDE_API_KEY`             | `""`                  | Anthropic API key.                                       |
| `CLAUDE_BASE_URL`            | `"https://api.anthropic.com"` | Override for custom Claude-compatible endpoints. |
| `SYSTEM_PROMPT_FILE_PATH`    | `""`                  | Path to a file whose content is injected as system prompt.|
| `SYSTEM_PROMPT_MODE`         | `"append"`            | `"overwrite"` or `"append"`.                             |
| `MAX_ERROR_COUNT`            | `3`                   | Consecutive errors before a provider is marked unhealthy.|
| `AUTO_HEALTH_CHECK_ENABLED`  | `false`               | Enable background health checks for unhealthy providers. |

### CLI flags

All JSON fields have a corresponding CLI flag (lower-case, hyphen-separated):

```
--port                        <number>
--host                        <address>
--api-key                     <key>
--model-provider              <provider[,provider...]>
--openai-api-key              <key>
--openai-base-url             <url>
--claude-api-key              <key>
--claude-base-url             <url>
--gemini-oauth-creds-base64   <base64>
--gemini-oauth-creds-file     <path>
--project-id                  <id>
--system-prompt-file          <path>
--system-prompt-mode          overwrite|append
--provider-pools-file         <path>
--max-error-count             <number>
--auto-health-check           true|false
```

### Provider pools file (`configs/provider_pools.json`)

For multi-credential setups, define pools in a separate JSON file:

```json
{
  "gemini-cli-oauth": [
    {
      "uuid": "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
      "isHealthy": true,
      "GEMINI_OAUTH_CREDS_FILE_PATH": "/path/to/creds1.json",
      "PROJECT_ID": "my-gcp-project"
    }
  ],
  "openai-custom": [
    {
      "uuid": "yyyyyyyy-yyyy-yyyy-yyyy-yyyyyyyyyyyy",
      "isHealthy": true,
      "OPENAI_API_KEY": "sk-..."
    }
  ]
}
```

Point to it with `--provider-pools-file configs/provider_pools.json` or via `PROVIDER_POOLS_FILE_PATH`.

---

## Supported endpoints

### OpenAI-compatible

| Method | Path | Description |
|--------|------|-------------|
| `GET`  | `/v1/models` | List all models from all healthy providers |
| `POST` | `/v1/chat/completions` | Chat completion (streaming and non-streaming) |
| `POST` | `/v1/responses` | OpenAI Responses API format |

### Anthropic Claude-compatible

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/messages` | Claude messages API |
| `POST` | `/v1/messages/count_tokens` | Token count estimate |

### Google Gemini-compatible (v1beta)

| Method | Path | Description |
|--------|------|-------------|
| `GET`  | `/v1beta/models` | List models (Gemini format) |
| `POST` | `/v1beta/models/:model:generateContent` | Non-streaming generation |
| `POST` | `/v1beta/models/:model:streamGenerateContent` | Streaming generation |

### Ollama-compatible

| Method | Path | Description |
|--------|------|-------------|
| `GET`  | `/api/version` | Ollama version (`0.12.10`) — no auth required |
| `GET`  | `/api/tags` | List all models with provider prefix labels |
| `POST` | `/api/chat` | Ollama chat (newline-delimited JSON streaming) |
| `POST` | `/api/generate` | Ollama legacy text generation |
| `POST` | `/api/show` | Model information |

All Ollama endpoints are also available under the `/ollama/` prefix (e.g. `/ollama/api/chat`).

### Provider-prefixed routing

Each provider type can be targeted directly:

```
POST /<provider>/v1/chat/completions
POST /<provider>/v1/messages
GET  /<provider>/v1/models
```

Supported `<provider>` values: `gemini-cli-oauth`, `gemini-antigravity`, `openai-custom`,
`openaiResponses-custom`, `claude-custom`, `claudeCode-custom`.

### Health

| Method | Path | Description |
|--------|------|-------------|
| `GET`  | `/health` | Liveness probe — always 200 OK |
| `GET`  | `/provider_health` | Per-provider health status and statistics |

---

## Provider types

| Provider type | Protocol | Authentication |
|--------------|----------|---------------|
| `gemini-cli-oauth` | OpenAI-compatible (CloudCode endpoint) | Google OAuth2 credentials file or base64 |
| `gemini-antigravity` | OpenAI-compatible (internal endpoint) | Google OAuth2 credentials file or base64 |
| `openai-custom` | OpenAI `/v1/chat/completions` | API key (`OPENAI_API_KEY`) |
| `openaiResponses-custom` | OpenAI `/v1/responses` | API key (`OPENAI_API_KEY`) |
| `claude-custom` | Anthropic `/v1/messages` | API key (`CLAUDE_API_KEY`) |
| `claudeCode-custom` | Anthropic Claude Code | API key (`CLAUDE_API_KEY`) |

---

## Ollama model name prefixes

When listing models via `/api/tags`, each model name is prefixed with its provider label so clients
can identify the source:

| Prefix | Provider |
|--------|----------|
| `[Gemini CLI] model-name` | `gemini-cli-oauth` or `gemini-antigravity` |
| `[Claude] model-name` | `claude-custom` or `claudeCode-custom` |
| `[OpenAI] model-name` | `openai-custom` or `openaiResponses-custom` |

When sending a request to `/api/chat` or `/api/generate`, the prefix is stripped automatically
and the correct provider is inferred from the model name.
