# Changelog

## Unreleased - 2026-07-12

### Added

- Added native Claude Code WebSearch skill support through Kiro's regional MCP
  endpoint, including streaming and non-streaming Messages responses, domain
  filters, retry and rate-limit handling, and usage accounting.
- Embedded the complete Admin Web UI in the Go binary. Deployments now need
  only the executable and no longer require a separate `web/` directory.

### Changed

- Replaced the traffic page's token trend line chart with an adaptive stacked
  bar chart with clearer axes, time buckets, hover details, and responsive
  desktop and mobile layouts.
- Preserved typed Claude server-tool metadata and readable server-tool history
  without translating unsupported Advisor or server tools into Kiro client
  tools.
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
