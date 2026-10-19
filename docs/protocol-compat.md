# Protocol compatibility repair (1.1.11)

This release changes generic protocol handling, not provider identity or
detector-specific answers. Account, proxy-pool and delivered-USD accounting
configuration remain unchanged.

## Structured output integrity

The adapter validates the entire generated JSON value. A broken outer object
must not be replaced with one valid nested object, and multiple JSON values must
not silently become the first value. One unambiguous JSON response wrapped in
ordinary prose or a Markdown fence is still normalized without re-encoding its
data, including large integers and literal thinking-tag strings.

An invalid complete answer follows the existing single bounded correction
attempt before any assistant content is committed. A second invalid answer
returns the existing typed upstream format error, not a fabricated success.
Native tool turns, refusals and truncated output retain their own semantics.
Chat Completions and Responses also reject invalid or unsupported JSON Schema
definitions before invoking an upstream account.

## Streaming and non-streaming stop semantics

Anthropic `refusal` and `pause_turn` are preserved instead of being rewritten to
`end_turn`. Existing context-window, length, pending client-tool and locally
matched stop-sequence precedence is unchanged. The same resolver serves normal
Messages responses, ordinary SSE and the Claude Code buffered stream.

Regression tests validate event names/types, serial block indexes, complete tool
JSON, terminal event order, cumulative usage, retry isolation and partial-stream
errors. Non-stream tests cover envelopes, tools, cache-token split, inflation
and typed HTTP errors. These are protocol tests, not a claim of a particular
CCTest score.

References:
- [Anthropic streaming contract](https://platform.claude.com/docs/en/build-with-claude/streaming)
- [Anthropic stop reasons](https://platform.claude.com/docs/en/build-with-claude/handling-stop-reasons)
- [Anthropic structured outputs](https://platform.claude.com/docs/en/build-with-claude/structured-outputs)
