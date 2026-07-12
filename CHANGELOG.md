# Changelog

## Unreleased - 2026-07-12

### Added

- Added native Claude Code WebSearch skill support through Kiro's regional MCP
  endpoint, including streaming and non-streaming Messages responses, domain
  filters, retry and rate-limit handling, and usage accounting.
- Embedded the complete Admin Web UI in the Go binary. Deployments now need
  only the executable and no longer require a separate `web/` directory.
- Added reasoning-effort forwarding for Claude Messages, Chat Completions, and
  Responses requests, including adaptive thinking and budget-based effort
  mapping for supported Kiro models.
- Added compatible tool-choice, stop-sequence, and structured JSON output
  handling across the Claude and OpenAI request adapters.

### Changed

- Replaced the traffic page's token trend line chart with an adaptive stacked
  bar chart with clearer axes, time buckets, hover details, and responsive
  desktop and mobile layouts.
- Preserved typed Claude server-tool metadata and readable server-tool history
  without translating unsupported Advisor or server tools into Kiro client
  tools.
- Aligned Claude, Chat Completions, Responses, and WebSearch response envelopes
  and streaming event order with their native protocols, including model ID
  echoing, reasoning signatures, citations, terminal status, and token details.
- Aligned Docker and standalone release builds on stripped, reproducible
  `-trimpath` binaries.
- Retired the embedded legacy Admin entry point; old
  `/admin/index-legacy.html` bookmarks now redirect to the maintained UI.

### Fixed

- Serialized asynchronous account-stat persistence so older snapshots cannot
  overwrite newer totals, synchronized account reloads with pending writes,
  coalesced bursts, and retried transient persistence failures.
- Added loading, empty, and error states to the traffic chart and hardened Web
  Search error propagation, response limits, proxy use, and account failover.
- Accepted both dotted and dashed Claude model version IDs, empty user turns,
  and thinking budgets that meet or exceed `max_tokens` without local 400
  errors.
- Hardened AWS event-stream parsing with frame and CRC validation, bounded
  parallel tool accumulation, exact large-number preservation, and typed
  failures for malformed or incomplete upstream tool JSON.
