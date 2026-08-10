# kiro.rs Upstream Port Record

- branch: `codex/port-kiro-rs-upstream`
- changed field: Kiro upstream forwarding, Anthropic conversion, streaming response contracts, bearer refresh, and quota failover
- baseline: `4a141f8748c1952fd02b36223cb241af59f7ae63`
- reference: `hank9999/kiro.rs` commit `5ca5703a57a970f0707108cd866b02d4f0f765cb`
- source checkout: clean local clone of the reference commit

## Ported Contracts

- Claude, Claude Code, OpenAI Chat, and Responses routes share the Kiro Event Stream parser while preserving each downstream event schema. Web-search MCP retries one forced OAuth bearer refresh per request and classifies monthly quota exhaustion as an immediate account disable.
- Claude conversion trims assistant prefill, accepts Claude Code session UUID metadata, merges adjacent roles, closes buffered user history with `OK`, preserves native history/tool rounds, normalizes schemas, adds Write/Edit chunking instructions, and shortens long tool names to 63 UTF-8-safe bytes with a digest.
- `/cc/v1/messages` and `/cc/v1/messages/count_tokens` are authenticated routes. Claude Code streaming buffers generated SSE events like `BufferedStreamContext`, forwards the first ping immediately and periodic keep-alives while waiting, then patches `message_start.input_tokens` from the terminal `kiro_context_usage` event before releasing the buffered response.
- Ordinary `/v1/messages` remains low-latency: the default early prelude sends `message_start` and `ping` after upstream headers, with `KIRO_EARLY_STREAM_PRELUDE=false` as an opt-out. The default SSE flush window is `0ms`, matching kiro.rs event-by-event delivery; operators can still opt into coalescing with `KIRO_SSE_FLUSH_WINDOW_MS`.
- Thinking tags use cross-frame buffering, quote/punctuation-aware boundaries, a single leading-newline trim, and hidden reasoning is never accepted as visible output.
- Claude, Chat, and Responses stream/non-stream attempts accept a clean terminal event only after client-visible text, requested reasoning, or a completed tool call is observed. Stop-sequence filtering runs before this decision. Empty attempts are retried with usage settlement and finish as `upstream_empty_response` rather than a fabricated empty success.
- A bounded continuation timeout can close an already-visible stream, but role envelopes, ping events, private marker probes, filtered stop text, and incomplete tool fragments do not count as visible output.
- Bearer-invalid `401/403` responses trigger one forced OAuth refresh and same-account retry for generation and MCP/WebSearch requests; API-key credentials skip OAuth refresh. `402` responses with `MONTHLY_REQUEST_COUNT` disable the exhausted account, while unrelated WebSearch `402` responses retain their cooldown behavior.
- MCP/WebSearch error bodies are bounded and retained internally for status and reason classification without exposing upstream response text to clients.
- Go retains strict AWS Event Stream CRC, malformed JSON, tool JSON, and upstream error-frame handling instead of converting protocol failures into successful terminal events.

## Verification

Run from `E:\KIROGO`:

```text
go test ./proxy -count=3                                      -> ok   kiro-go/proxy 61.552s
go test -p 1 . ./auth ./config ./logger ./pool ./proxy ./scripts ./web -count=1
                                                               -> all selected packages passed (auth 1.575s; config 2.476s; pool 0.228s; proxy 19.654s)
go vet ./proxy                                                -> exit 0, no output
go build .                                                    -> exit 0, no output
gofmt -l <changed Go files>                                   -> exit 0, no output
git diff --check                                              -> exit 0 (line-ending warnings only)
```

The serial full `go test -p 1 ./... -count=1` check still reaches the pre-existing untracked package `exports/reasoning-effort-20260808`, whose `MODIFIED_FILE.go` reports undefined `MapModel`, `ClaudeRequest`, `isClaudeThinkingRequested`, `OpenAIRequest`, `ParseModelAndThinking`, and `ClaudeThinkingConfig`. The failure is isolated from `proxy` and the main binary; that untracked fixture was preserved.

The baseline run was executed from a detached worktree at `4a141f8` and returned `ok   kiro-go/proxy 13.971s`. `ROLLBACK.sh` restored `exports/kiro-rs-port-20260810/rollback/handler-target-actual.txt` to the baseline SHA-256 `0A28375872617F005E45C5519B59ACC4154D4103D65010486FD7AA15428098AD`.

The `/cc` endpoint intentionally retains kiro.rs' full-response buffering contract. Its per-request memory use therefore grows linearly with the buffered response size; ordinary `/v1/messages` does not take this path.

## Files

- `proxy/claude_cc_buffered.go`
- `proxy/claude_cc_buffered_test.go`
- `proxy/kiro_rs_contract_test.go`
- `proxy/kiro_rs_conversion_test.go`
- `proxy/kiro_rs_conversion_extra_test.go`
- `proxy/kiro_rs_streaming_test.go`
- `proxy/websearch.go`
- `proxy/websearch_test.go`
- `exports/kiro-rs-port-20260810/MIGRATION.md`
