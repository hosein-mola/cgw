# Codex ArvanCloud Proxy

**New: management CLI.** Save/update provider keys, run a background server, inspect logs,
and install or restore Codex settings. See [CLI.md](CLI.md) for Windows, Linux, and macOS
setup, credential storage, model switching, backups, and rollback.

```text
proxy init
proxy set-key YOUR_ARVAN_API_KEY
proxy start
proxy models
proxy run
```

```powershell

 .\bin\proxy.exe list
  .\bin\proxy.exe clear
  .\bin\proxy.exe models
  .\bin\proxy.exe history
  .\bin\proxy.exe resume SESSION_ID
  .\bin\proxy.exe set-key APIKEY
  .\bin\proxy.exe del-key
  .\bin\proxy.exe ls-key
  .\bin\proxy.exe check
  .\bin\proxy.exe run
  .\bin\proxy.exe run codex
```

Use `./proxy` on Linux/macOS or `.\proxy.exe` on Windows unless the binary is on PATH.

A small Go proxy that gives OpenAI Codex CLI a Responses API while using curated coding models through ArvanCloud's Chat Completions API.

The proxy translates requests, streaming text, tool calls, tool results, usage, finish reasons, and errors. Provider `data: [DONE]` markers are consumed internally; every successful downstream stream ends with `response.completed` (or `response.incomplete` for a length limit).

## Features

- `POST /v1/responses`, including streaming and non-streaming clients
- Codex function tools, incremental arguments, multiple calls, and `function_call_output` continuation
- Codex skills plus freeform/custom tools such as native `apply_patch`, bridged through Chat-compatible function calls
- Per-model upstream routing, including native Responses pass-through for Responses-only models such as `GPT-5.2-Codex`
- ArvanCloud-only routing with a stable local Responses API
- Upstream streaming or configurable non-streaming fallback
- Constant-time bearer authentication and separate provider credentials
- Request-size/header limits, cancellation propagation, stream idle timeout, structured logs, and graceful shutdown
- Local-only binding by default

## Configure

Copy `.env.example` values into your shell or secret manager. The YAML stores only environment-variable names, never credentials.

PowerShell:

```powershell
$env:PROXY_API_KEY = "choose-a-long-random-local-secret"
$env:ARVAN_API_KEY = "your-arvan-key"
```

The original deployment variable name `ARVANAI_KEY` is accepted as a fallback alias. `ARVAN_API_KEY` takes precedence.

Review [config.yaml](config.yaml) to change routing, timeouts, limits, or upstream streaming. The default shared ArvanCloud endpoint is already configured. If the ArvanCloud dashboard gives you a model-specific gateway URL, place the complete URL ending in `/v1` in `providers.arvan.base_url`, for example:

```yaml
models:
  deepseek-v4-pro:
    provider: arvan
    upstream_model: DeepSeek-V4-Pro

providers:
  arvan:
    base_url: "https://arvancloudai.ir/gateway/models/DeepSeek-V4-Pro/YOUR-GATEWAY-ID/v1"
    api_key_env: "ARVAN_API_KEY"
    upstream_stream: true
```

Keep the gateway model and `upstream_model` aligned. Supply the base URL through `/v1`; the proxy adds `/chat/completions`. Setting `upstream_stream` to `false` makes the proxy request one Chat completion and synthesize the Responses lifecycle locally.

## Run on Windows

Go 1.24 or newer is required.

```powershell
go test ./...
go run ./cmd/proxy -config config.yaml
```

Health checks do not contact providers:

```powershell
curl.exe http://127.0.0.1:3002/health
curl.exe http://127.0.0.1:3002/health/providers
```

Test a Responses stream before starting Codex:

```powershell
$body = @{
  model  = "deepseek-v4-pro"
  input  = "Say hi"
  stream = $true
} | ConvertTo-Json -Compress

curl.exe -N "http://127.0.0.1:3002/v1/responses" `
  -H "Authorization: Bearer $env:PROXY_API_KEY" `
  -H "Content-Type: application/json" `
  --data-raw $body
```

The final SSE event should be `response.completed`.

## Codex configuration

Add this to the Codex config:

```toml
model = "deepseek-v4-pro"
model_provider = "arvan_proxy"

[model_providers.arvan_proxy]
name = "ArvanCloud Proxy"
base_url = "http://127.0.0.1:3002/v1"
env_key = "PROXY_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
```

List or change the default Codex model without changing providers:

```powershell
.\bin\proxy.exe models
.\bin\proxy.exe run
.\bin\proxy.exe run codex
```

`models` fetches ArvanCloud's live catalog, filters it to strong coding and agent
models, groups the available choices as Cheap, Medium, and Frontier, then shows one
numbered menu and saves the choice. All models in that menu are added to the local
Arvan routing configuration. `run` starts Codex with that saved model.
`run codex` uses the built-in OpenAI provider and your existing ChatGPT subscription.
`history` lists every local Codex session with its full ID, timestamp, provider, working
directory, and exact resume command. OpenAI sessions use `codex resume SESSION_ID`;
managed Arvan sessions use `proxy resume SESSION_ID` so the wrapper can supply the local
proxy environment.
Use `clear` to clear the interactive console or the current terminal on Windows,
Linux, and macOS.

If an upstream stream fails after the first downstream event, the proxy emits `response.failed` with a useful error.

## Docker

Build and bind only to loopback:

```powershell
docker build -t codex-deepseek-proxy .
docker run --rm `
  -p 127.0.0.1:3002:3002 `
  -e PROXY_API_KEY=$env:PROXY_API_KEY `
  -e ARVAN_API_KEY=$env:ARVAN_API_KEY `
  codex-deepseek-proxy
```

The image listens on all interfaces inside its container via `PROXY_HOST`; the host-side `127.0.0.1` publish keeps it local to your machine.

## Security and operational behavior

- `/v1/models` and `/v1/responses` require `Authorization: Bearer ...`; health endpoints reveal only readiness/configuration booleans.
- Prompts, tool output, authorization headers, and keys are never logged.
- HTTP 408/429/5xx and transient network failures are retried once. Validation and authentication errors are not retried.
- Client disconnects cancel the provider request. A silent upstream stream is canceled after `idle_stream_seconds`.
- Provider support can be checked without exposing keys at `/health/providers`.

## Test scope

Unit tests cover request/tool conversion, custom/freeform tool wrapping, Codex custom-tool events and history, SSE framing (LF, CRLF, multiline and partial reads), reasoning suppression, text completion, tool argument accumulation, call-ID preservation, authentication, and model listing. Live provider tests require real credentials and are intentionally not run by `go test`.
