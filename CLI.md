# Manage the proxy and Codex

Build once with Go 1.24+ and use the resulting executable. Background servers must be
started from a persistent binary; do not delete or move it while it is running.

| Platform | Build | Invoke |
| --- | --- | --- |
| Windows PowerShell | `go build -o bin/proxy.exe ./cmd/proxy` | `.\bin\proxy.exe` |
| Linux | `go build -o bin/proxy ./cmd/proxy` | `./bin/proxy` |
| macOS (Intel or Apple Silicon) | `go build -o bin/proxy ./cmd/proxy` | `./bin/proxy` |

Examples below use `proxy`; substitute the invocation above or put the binary on PATH.
Run `proxy list` for the complete command list. Global `--home` and `--config` flags
go **before** the command. Key input and Codex options go after their subcommand.

To build all five Windows/Linux/macOS targets, run `./scripts/build.ps1` in PowerShell
or `sh scripts/build.sh` in a POSIX shell. Outputs go to `bin/`; Windows targets end
in `.exe`, Linux targets are named `linux`, and macOS targets are named `darwin`.

## First setup

```text
proxy init
proxy set-key YOUR_ARVAN_API_KEY
proxy ls-key
proxy start
proxy status
proxy codex install
proxy models
proxy history
proxy run
```

`set-key APIKEY` stores the ArvanCloud credential. `init` creates a random local proxy
authentication key and copies the embedded server config; running it again preserves
existing settings and keys.

The embedded config uses ArvanCloud's shared API URL. If your ArvanCloud dashboard
provides a model-specific gateway URL, set that complete URL through `/v1` as
`providers.arvan.base_url` in the managed `config.yaml`. The proxy appends
`/chat/completions`; keep `models.*.upstream_model` equal to the gateway model name.

The wrapper requires Codex CLI on PATH. It installs model profiles, starts the server
if needed, and passes only the proxy key to the Codex child environment. It does not
edit shell startup scripts or global environment variables. The server stays running
after Codex exits; `proxy stop` shuts it down. Existing ChatGPT login data is untouched.

## Update or remove credentials

```text
proxy set-key YOUR_NEW_ARVAN_API_KEY
proxy ls-key
proxy del-key
proxy restart
```

Stored keys take precedence over environment variables. Updates apply when the server
restarts. After changing the local proxy key, restart any Codex session too.
`ls-key` reports only whether the Arvan API key is stored; it never displays the value.

## Storage

| OS | Default management directory | Credential protection |
| --- | --- | --- |
| Windows | `%APPDATA%\codex-deepseek-proxy` | Current-user DPAPI encryption and a current-user-only ACL |
| Linux | `$XDG_CONFIG_HOME/codex-deepseek-proxy`, otherwise `~/.config/codex-deepseek-proxy` | Owner-only directory (`0700`) and files (`0600`) |
| macOS | `~/Library/Application Support/codex-deepseek-proxy` | Owner-only directory (`0700`) and files (`0600`) |

On Linux and macOS the credential file is permission-protected JSON, **not encrypted**;
this implementation does not use Keychain or Secret Service. Windows credentials are
bound to the Windows user and machine. Re-enter keys when migrating platforms.
Protect this directory as secret data. Secrets are separate from `config.yaml` and
Codex TOML. Known stored/environment keys are redacted from managed logs.

Override paths for an isolated installation:

```text
proxy --home /path/to/private-state init
proxy --home /path/to/private-state --config /path/to/config.yaml start
```

Use the same `--home` for subsequent commands. The `PROXY_HOST` environment variable
can override the server bind address. When generating Codex settings, set the desired
client address and port in the YAML; wildcard binds become loopback client URLs.

## Server, logs, errors, and checks

```text
proxy start
proxy status
proxy logs --lines 100
proxy logs --errors
proxy logs --follow
proxy doctor
proxy check
proxy check gpt-5-2-codex gemini-3-1-pro-preview
proxy restart
proxy stop
```

`start` detaches the server (hidden on Windows) and waits for readiness. `status`,
`stop`, and `restart` authenticate to a separate loopback-only control endpoint with
a private instance token. They never kill a process merely because its PID matches.
Shutdown gives active requests ten seconds, then closes remaining connections.
Stale state after a crash can be replaced on the next start. Management changes are
serialized with lock files. If a CLI process crashes while holding a lock, the error
names the exact lock; remove it only after verifying no management command is running.

Logs rotate automatically at roughly 5 MiB with one previous file (`server.log.1`).
`--follow` handles rotation. `--errors` shows ERROR-level records in the current file.
`doctor` checks local configuration and reports availability without contacting providers.
`check` makes one small **paid** request per configured coding model and requires each
model to produce a Responses custom tool call. Pass proxy or upstream model names to retest only
specific models. The required-tool request normally ends immediately after the call, and
does not impose a legacy Chat token-limit field that some newer models reject. Foreground
operation remains available as `proxy serve`.
`start` is a user-session process, not a system service or login/autostart installation.

## Models and automatic Codex setup

```text
proxy models
proxy history
proxy resume SESSION_ID
proxy codex install
proxy run
proxy run codex
proxy codex run --model deepseek-v4-pro -- "Inspect this project"
```

`install` registers the local Responses provider and creates one Codex-safe profile
file per configured ArvanCloud model. It leaves the current default model in place.
`models` fetches the live catalog with the stored Arvan API key, keeps the curated
coding and agent choices that Arvan currently offers, groups them as Cheap, Medium,
and Frontier, displays one numbered list, saves the chosen model, and configures Codex
for it. Every displayed model is added to the local Arvan routing configuration.
Plain `run` starts Codex with the saved selection, while `run codex` selects the
built-in OpenAI provider and starts Codex with the existing ChatGPT login.
The switch replaces the Arvan catalog override with `codex.json` in the Codex home
when present, or clears the override to use Codex's default catalog. Arvan profiles
select `arvan-models.json` independently, so switching takes effect on the first launch.

`history` reads only the metadata object of each local Codex rollout. It lists the full
session ID, timestamp, provider, working directory, and exact command without printing
prompts or responses. OpenAI sessions use `codex resume SESSION_ID`. Managed Arvan
sessions use `proxy resume SESSION_ID`; the wrapper starts the local proxy when needed
and passes the ID to Codex's supported `resume` command. Quote an optional prompt after
the ID if you want to continue the session immediately.

Running `proxy` without a command opens an interactive console. It prints the
grouped command list and stays open at a `proxy>` prompt until `exit` or `quit`.
Type `clear` in that console, or run `proxy clear`, to clear Command Prompt,
PowerShell, or a Linux/macOS terminal.

This follows the current [Codex profile-file format](https://learn.chatgpt.com/docs/config-file/config-advanced)
(Codex 0.134+). Earlier Codex versions used inline `[profiles.NAME]` tables; upgrade
Codex before using the generated profiles. These changes do not weaken sandbox,
approval, trust, or tool permissions. Existing project and CLI overrides may still
take precedence over model settings.

Prefer `proxy codex run` when using stored credentials. Direct `codex` additionally
needs the proxy key in its environment; the wrapper supplies that automatically. The
CLI does not save bearer tokens in Codex config.

## Backups, ChatGPT, and rollback

```text
proxy codex chatgpt
proxy codex backups
proxy codex restore
proxy codex restore --backup SNAPSHOT.toml
```

- `chatgpt` selects the built-in OpenAI provider, removes the explicit model and
  legacy profile selector, and clears an `openai_base_url` override. Codex selects
  its normal default model. Your existing ChatGPT login remains in place; if you
  were never logged in, use `codex login`.
- `restore` recovers the exact original config, including comments and line endings,
  and rolls back generated profiles. If a file originally did not exist, it is removed.
- `restore --backup NAME` restores that particular **base config** snapshot; it does
  not roll the profile files back to the snapshot date.

Codex config defaults to `$CODEX_HOME/config.toml`, otherwise `~/.codex/config.toml`
on every OS. Use `--config-file /path/to/config.toml` on any `codex` subcommand to
target another installation. Auth files and keychain entries are never edited.

Every changed file gets an exact snapshot under `.deepseek-proxy-backups/FILE/` next
to the config. The first baseline remains intact across repeated installs and switches.
Writes use a synced temporary file and atomic replacement. TOML serialization preserves
settings but normalizes formatting and comments in edited files; exact originals remain
in backups. Backups are private, since an existing config might contain sensitive data.

Malformed TOML and collisions with existing unmanaged provider/profile names are refused.
Outside edits are detected before rollback. Review those edits and then use
`proxy codex restore --force` only if you intend to replace them; the current files
are backed up first, including malformed files. Profile installation and rollback
operate on several files, each replaced atomically; an OS interruption can leave a
partial operation. The per-file backup indexes allow retrying rollback safely.

## Validation

`go test ./...` covers credentials, exact rollback, drift/collision protection, existing
settings/login preservation, environment precedence, and control-address validation,
alongside the existing proxy protocol tests. Windows lifecycle commands are smoke-tested
with an isolated management directory. Linux and macOS binaries are cross-compiled;
runtime validation on those platforms requires a native machine.
