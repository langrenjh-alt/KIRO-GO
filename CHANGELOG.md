# Changelog

## Unreleased - 2026-07-12

### Added

- Added a reversible local protocol-compatibility layer for Kiro thinking
  signatures and WebSearch opaque values. It rebuilds the observed Protobuf
  envelope shape with request-local metadata and can be disabled with
  `KIROGO_PROTOCOL_CAMOUFLAGE=false`.
- Added native Claude Code WebSearch skill support through Kiro's regional MCP
  endpoint, including streaming and non-streaming Messages responses, domain
  filters, retry and rate-limit handling, and usage accounting.
- Added mixed-tool WebSearch routing, native Responses API `web_search_call`
  output, and two-stage Advisor execution with paired Claude server-tool blocks.
- Embedded the complete Admin Web UI in the Go binary. Deployments now need
  only the executable and no longer require a separate `web/` directory.
- Added reasoning-effort forwarding for Claude Messages, Chat Completions, and
  Responses requests, including adaptive thinking and budget-based effort
  mapping for supported Kiro models.
- Added compatible tool-choice, stop-sequence, and structured JSON output
  handling across the Claude and OpenAI request adapters.

### Changed

- Forward complete conversation payloads to Kiro without the former 900 KiB
  history truncation, and return Kiro's `Input is too long` response directly
  instead of compacting and retrying the request.
- Cap each response's combined `input_tokens`, `cache_read_input_tokens`, and
  `cache_creation_input_tokens` at the selected model's context window; traffic
  page totals remain cumulative across requests.

- Replaced the traffic page's token trend line chart with an adaptive stacked
  bar chart with clearer axes, time buckets, hover details, and responsive
  desktop and mobile layouts.
- Preserved typed Claude server-tool metadata and readable server-tool history,
  including Advisor model settings and paired server-side result blocks.
- Aligned Claude, Chat Completions, Responses, and WebSearch response envelopes
  and streaming event order with their native protocols, including model ID
  echoing, reasoning signatures, citations, terminal status, and token details.
- Streamed Anthropic structured JSON incrementally from its first object or
  array delimiter while filtering surrounding prose and Markdown; non-stream
  schema mismatches now retry once on the same healthy account.
- Aligned Docker and standalone release builds on stripped, reproducible
  `-trimpath` binaries.
- Retired the embedded legacy Admin entry point; old
  `/admin/index-legacy.html` bookmarks now redirect to the maintained UI.

### Fixed

- Kept successful empty upstream turns as valid empty Claude messages instead
  of converting them into `empty_visible_output` gateway errors or account
  cooldowns; treated non-thinking reasoning events consistently as visible text.
- Kept Claude usage and prompt-cache metadata anchored to client-visible input
  tokens instead of Kiro's hidden editor context; added Anthropic request IDs
  and accepted standard native `web_search_*` queries alongside Claude Code.
- Normalized Claude/OpenAI structured-output responses to a single JSON value,
  including streaming responses, and removed non-stream tool-call narration
  when no stop sequence is active.
- Separated durable refill eligibility from transient runtime availability so
  concurrency saturation and short cooldowns cannot trigger external account
  replenishment.
- Serialized asynchronous account-stat persistence so older snapshots cannot
  overwrite newer totals, synchronized account reloads with pending writes,
  coalesced bursts, and retried transient persistence failures.
- Added loading, empty, and error states to the traffic chart and hardened Web
  Search error propagation, response limits, proxy use, and account failover.
- Accepted both dotted and dashed Claude model version IDs, empty user turns,
  and thinking budgets that meet or exceed `max_tokens` without local 400
  errors.
- Accepted every positive enabled-thinking budget, including protocol probes
  below the legacy 1024-token minimum, while preserving the requested value in
  the Kiro thinking prompt.
- Hardened AWS event-stream parsing with frame and CRC validation, bounded
  parallel tool accumulation, exact large-number preservation, and typed
  failures for malformed upstream tool JSON; accepted complete tool JSON when
  Kiro omits the terminal `stop=true` fragment.
- Truncated oversized WebSearch context to a Unicode-safe query tail and
  preferred the final Claude Code search prefix instead of returning a local
  2048-character validation error.
- Allowed explicit admin account tests to bypass transient routing cooldowns,
  quota gates, and cached model filters while still honoring concurrency limits.
- Normalized Claude message, client-tool, and server-tool IDs to fixed-width
  opaque protocol shapes; stopped adding a duplicate `x-request-id` behind an
  outer gateway and exposed the standard `request-id` response header.
- Aligned final Claude usage metadata with native `message_delta` responses,
  including zero thinking-token details for plain turns; suppressed narrated
  text before tool calls and limited `tool_choice:any` to one selected call.
