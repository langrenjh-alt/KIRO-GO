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
- Added named Prompt Cache policies with create, edit, delete, live reload, and
  per-API-key binding in both the Admin API and embedded Web UI.
- Added stage-level TTFT histograms to `/v1/stats` for request-body processing,
  configuration/cache/account waits, connection and TLS setup, upstream headers
  and first events, and the first client-visible token flush.

### Changed

- Kiro generation requests now use only the regional
  `q.<region>.amazonaws.com` endpoint; endpoint-level fallback has been removed.
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
- Moved API-key usage accounting off the request-time configuration write path;
  counters now accumulate atomically in memory, flush every five seconds, retry
  failed writes, and perform a final flush during graceful shutdown.
- Reduced burst-request memory by streaming bounded JSON decoding wherever raw
  request bytes are not required. Function-cache simulation retains its existing
  byte-level request fingerprint behavior.
- Reduced Prompt Cache tail latency with bounded expiry maintenance, background
  cleanup, lock-independent persistence snapshots, function-cache expiry heaps,
  named-policy preload, and single-flight lazy recovery.
- Kept global and per-API-key request concurrency unrestricted. Connection-pool
  tuning limits idle reuse only and does not cap active upstream connections.
- Left streaming response-header waits unlimited by default so a slow but valid
  upstream first response is not aborted; operators can opt in with
  `KIRO_RESPONSE_HEADER_TIMEOUT_SECONDS`.
- Coalesced duplicate Profile ARN discovery per account and synchronized newly
  resolved ARNs into the live pool; upstream retries now add jitter, honor
  `Retry-After`, and share a process-wide retry budget.
- Kept the first SSE events immediately flushed while coalescing later events in
  a bounded window; downstream writes now have per-write deadlines and cancel
  their upstream request on failure.

### Fixed

- Replaced flattened historical `Tool results:` dialogue with an explicit
  untrusted-data envelope, and scrubbed model-authored `user Tool results ...
  assistant` transcript copies before validated structured tool calls are
  exposed. Tool-free final answers and ordinary prose remain untouched.
- Parsed Kiro's terminal `metadataEvent.stopReason`. Clean Enterprise/IdC
  streams that omit metadata use their final `meteringEvent` as a terminal
  fallback, while `MAX_TOKENS`, missing coding-agent marker/tool evidence,
  interrupted agent output, and incomplete tools still enter bounded recovery.
- Corrected native Kiro stop-reason mapping so `END_TURN` remains a completed
  response in Chat Completions and Responses instead of being mislabeled as
  `length` or `incomplete`.
- Removed the five-minute wall-clock timeout from Kiro streaming requests so
  long-running agent turns are not cut off mid-response; downstream request
  cancellation still terminates abandoned upstream streams.
- Added a request-scoped completion protocol for recognized coding agents:
  reasoning is forwarded immediately, completed answers carry a stripped random
  marker, and unmarked assistant text remains pending until a real structured
  tool call confirms it as tool narration.
- Removed recovery action windows, marker-mismatch grace timers, and
  reasoning/pending-text byte ceilings. Marker prefix checks only control text
  visibility; normally correction is considered after the upstream stream ends
  with neither the marker nor a valid tool call.
- Limited only the initial coding-agent reasoning phase to 300 seconds after its
  first native reasoning event. A completion marker or structured tool event
  cancels the timer; expiry enters the existing semantic correction flow, while
  both correction attempts and ordinary requests remain untimed.
- Preserved upstream event order for successful tool turns across Claude
  Messages, Chat Completions, and Responses: reasoning, assistant text, then the
  structured tool call. Final answers that need no tool use the marker path and
  are released immediately after the internal marker is stripped.
- Limited missing-marker correction to two attempts while keeping transport and
  account retries separate. Repeated protocol failure returns an explicit
  `upstream_incomplete_response` without exposing buffered narration.
- Added bounded dial and TLS-handshake timeouts for tool follow-up requests
  without applying a whole-stream or model-thinking deadline; response-header
  timeout remains opt-in through `KIRO_RESPONSE_HEADER_TIMEOUT_SECONDS`.
- Compacted only long coding-agent requests after an actionless semantic failure:
  recovery keeps the system protocol, current request, recent user/tool evidence,
  and tool definitions while dropping old assistant planning from a fresh recovery
  conversation. Normal first attempts and short-request compatibility paths are
  unchanged.
- Added a UTF-8-safe 1 KiB action hint to semantic recovery requests. The hint
  retains only the failed attempt's tail, is JSON-escaped and marked as untrusted
  diagnostic context, and is replaced rather than duplicated on a later recovery;
  normal first attempts and transport-only retries remain unchanged.
- Returned a protocol-valid unavailable result when every account receives a
  permanent WebSearch MCP JSON-RPC rejection, while retaining failed-request
  metrics and classifying WebSearch and agent-protocol failures separately.
- Kept long-running Claude, Chat Completions, and Responses streams alive while
  Kiro is silent by forwarding a 25-second SSE heartbeat, matching kiro.rs
  without changing active-stream content or protocol metadata.
- Kept successful empty upstream turns as valid empty Claude messages instead
  of converting them into `empty_visible_output` gateway errors or account
  cooldowns; suppressed reasoning events when thinking output is not enabled.
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
