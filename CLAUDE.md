# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Kiro-Go is a single-binary Go reverse proxy that exposes Kiro (AWS CodeWhisperer) accounts
as OpenAI- and Anthropic-compatible APIs. It owns account pooling, OAuth token refresh,
SSE streaming translation, prompt caching, and an embedded admin web UI.

No third-party web framework: routing is a hand-written `switch` on `r.URL.Path`, and the only
dependencies are `google/uuid` and `santhosh-tekuri/jsonschema/v5`.

## Commands

Build the binary (do **not** use `./...` for builds — `exports/` contains scratch Go files that
do not compile):

```bash
go build -o kiro-go .
```

Vet the real packages:

```bash
go vet ./config ./pool ./auth ./proxy ./logger ./web .
```

Run tests (`./proxy` is the large suite, ~12s; the others are fast):

```bash
go test ./config ./pool ./auth ./proxy
```

Run a single test:

```bash
go test ./proxy -run TestClaudeStreamThinking -v
```

Run the server locally (config auto-created at `data/config.json`, admin at `/admin`):

```bash
go build -o kiro-go . && ./kiro-go
```

## Architecture

Request flow for a generation call:

1. `main.go` loads config, starts the API-key usage flusher, builds `proxy.NewHandler()`.
2. `proxy/handler.go` `ServeHTTP` dispatches on path. Generation paths authenticate via
   `proxy/auth.go`, which resolves a `config.ApiKeyEntry` into the request context.
3. `proxy/translator.go` converts the inbound `ClaudeRequest` / `OpenAIRequest` into a
   `KiroPayload` (`ClaudeToKiro`, `OpenAIToKiro`), applying model aliasing (`MapModel`),
   thinking-mode resolution, prompt filters, and tool-schema normalization.
4. The handler loops over accounts via `h.acquireAccountForModel` (`proxy/account_failover.go`),
   which leases from `pool.AccountPool` respecting per-account concurrency, cooldowns, and
   model availability.
5. `proxy/kiro.go` calls the regional `https://q.<region>.amazonaws.com/generateAssistantResponse`
   endpoint and parses the **AWS Event Stream** binary framing into callbacks.
6. Callbacks are re-encoded into the target protocol's SSE or JSON envelope and written out.

`proxy/handler.go` is ~6400 lines and holds the four protocol paths (Claude stream/non-stream,
OpenAI stream/non-stream) plus the entire admin API route table (`handleAdminAPI`, near line 4184).
Changes to one protocol usually need the parallel change in the other three.

### Packages

- `config` — the single source of persistent truth, one JSON document guarded by `cfgLock`.
  Because the whole document is rewritten on save, a cross-process lock
  (`config_process_lock*.go`, `ErrConfigPathInUse`) prevents two instances from clobbering
  each other. Request-path API-key usage counters deliberately bypass this: they accumulate
  atomically in `apikey_usage.go` and flush on a timer.
- `pool` — weighted round-robin / best-account selection with cooldowns, error counts,
  in-flight concurrency leases, and quota/overage gating. `Acquire*` must be paired with
  `Release`.
- `auth` — the login flows: AWS Builder ID device code (`builderid.go`), IAM Identity Center
  PKCE (`iam_sso.go`), the Kiro hosted portal used by enterprise Entra/Azure tenants
  (`kiro_sso.go`, needs a loopback listener on port 3128), raw SSO token import, and OIDC
  token refresh. Test seams live in `testhooks.go`.
- `proxy` — everything protocol-facing. Notable files beyond the four above:
  `cache_tracker.go` (prompt-cache simulation, persisted to `prompt_cache.json`),
  `responses_*.go` (OpenAI Responses API incl. stored responses under `data/responses/`),
  `websearch.go` / `advisor.go` (server-side tools routed through Kiro's MCP endpoint),
  `agent_completion.go` (detects and repairs narration-only / truncated agent turns),
  `payload_preflight.go` (size diagnostics that never retain prompt text),
  `protocol_camouflage.go` (rebuilds Kiro's Protobuf envelope for thinking signatures).
- `web` — the admin UI, embedded via `//go:embed` in `embed.go`. Deployments ship only the
  binary. Plain JS + browser Tailwind, no build step. All user-visible strings go through
  `data-i18n` attributes and must be added to **both** `locales/en.json` and `locales/zh.json`.

### Endpoints

Generation: `/v1/messages`, `/v1/chat/completions`, `/v1/responses` (plus unprefixed and
`/anthropic/v1/...` aliases), `/v1/messages/count_tokens`, `/v1/models`.
Ops: `/health`, `/v1/stats` (API-key auth). Admin: `/admin`, `/admin/api/*`
(auth via `X-Admin-Password` header or `admin_password` cookie).

## Conventions

Testing seams are function-typed fields on `Handler` — set `h.kiroCaller` and
`h.webSearchMCPCaller` to fake upstream calls rather than starting servers. Tests isolate
config with `config.Init(filepath.Join(t.TempDir(), "config.json"))`.

Comments are bilingual: existing Chinese comments stay, new explanatory comments are written
in English and explain *why* a constraint exists (token-domain choices, lock ordering,
why a timeout is 0). Match the density of the file you are editing.

Runtime tuning is env-var driven, not config-driven — see `proxy/runtime_tuning.go` for the
`KIRO_*_TIMEOUT_SECONDS` knobs, plus `KIROGO_PROTOCOL_CAMOUFLAGE`, `KIRO_PROFILE_REGIONS`,
`KIRO_SSE_FLUSH_WINDOW_MS`, `KIRO_RETRY_BUDGET_PER_SECOND`, `KIRO_MAX_RAW_BODY_BYTES`.
Deploy-facing vars are `CONFIG_PATH`, `ADMIN_PASSWORD`, `LOG_LEVEL`.

`http.Server` sets `WriteTimeout: 0` on purpose — SSE generations run for minutes. Don't
"fix" it.

Commits follow Conventional Commits with a package scope: `fix(proxy): ...`, `feat(admin): ...`,
`docs(deploy): ...`.

`ref_mie/`, `exports/`, and `dist/` are untracked local scratch: an upstream reference fork,
experiment artifacts, and build outputs. Never treat them as project source, and don't include
them in builds or vet runs.
