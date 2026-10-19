package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/costledger"
)

func attachQuotaTestLedger(t *testing.T, h *Handler) *costledger.Ledger {
	t.Helper()
	l := costledger.NewMemory()
	t.Cleanup(func() { _ = l.Close() })
	h.accountQuota = &accountQuotaController{ledger: l, now: time.Now, recent: map[string]time.Time{}, polled: map[string]time.Time{}, wake: make(chan struct{}, 1)}
	return l
}

func quotaLedgerTotal(h *Handler) (float64, uint64, uint64) {
	var usd float64
	var unknown, incomplete uint64
	for _, account := range config.GetAccounts() {
		e := h.accountQuotaSnapshot(account.ID)
		usd += e.SpentUSD
		unknown += e.UnpricedRequests
		incomplete += e.IncompleteRequests
	}
	return usd, unknown, incomplete
}

func TestQuotaReceiptLargestRemainder(t *testing.T) {
	for _, test := range []struct {
		total         int64
		weights, want []int64
	}{
		{5, []int64{1, 1, 1}, []int64{2, 2, 1}},
		{1, []int64{0, 10}, []int64{0, 1}},
		{math.MaxInt64, []int64{math.MaxInt64, math.MaxInt64}, []int64{4611686018427387904, 4611686018427387903}},
		{0, []int64{0, 0}, []int64{0, 0}},
	} {
		got, err := quotaLargestRemainder(test.total, test.weights)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%+v got=%v err=%v", test, got, err)
		}
	}
	for _, test := range []struct {
		total   int64
		weights []int64
	}{{1, nil}, {1, []int64{0}}, {-1, []int64{1}}, {1, []int64{-1}}} {
		if _, err := quotaLargestRemainder(test.total, test.weights); err == nil {
			t.Fatalf("accepted %+v", test)
		}
	}
}

func TestQuotaReceiptAllocationConservesAndMerges(t *testing.T) {
	final := costledger.Usage{InputTokens: 101, OutputTokens: 19, CacheReadTokens: 77, CacheWrite5mTokens: 29, CacheWrite1hTokens: 31}
	sources := []quotaSource{{AccountID: "a", Usage: costledger.Usage{InputTokens: 2, OutputTokens: 3, CacheReadTokens: 4, CacheWrite5mTokens: 5, CacheWrite1hTokens: 6}}, {AccountID: "b", Usage: costledger.Usage{InputTokens: 7, OutputTokens: 8, CacheReadTokens: 9, CacheWrite5mTokens: 10, CacheWrite1hTokens: 11}}, {AccountID: "a", Usage: costledger.Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWrite5mTokens: 4, CacheWrite1hTokens: 5}}}
	allocated, err := allocateQuotaUsage(final, sources)
	if err != nil || len(allocated) != 2 {
		t.Fatalf("allocation=%+v err=%v", allocated, err)
	}
	sum := make([]int64, 5)
	for _, part := range allocated {
		for i, n := range quotaCounts(part) {
			sum[i] += n
		}
	}
	if !reflect.DeepEqual(sum, quotaCounts(final)) {
		t.Fatalf("sum=%v final=%+v", sum, final)
	}
	if _, err := allocateQuotaUsage(final, []quotaSource{{Usage: final}}); err == nil {
		t.Fatal("missing account accepted")
	}
	if _, err := allocateQuotaUsage(final, []quotaSource{{AccountID: "a", Usage: costledger.Usage{InputTokens: -1}}}); err == nil {
		t.Fatal("negative accepted")
	}
	if _, err := quotaPromptTokens(costledger.Usage{InputTokens: math.MaxInt64, CacheReadTokens: 1}); err == nil {
		t.Fatal("overflow accepted")
	}
}

func TestQuotaReceiptRetryProvenance(t *testing.T) {
	var retry retryBillingUsage
	a := newDiscardedAttemptUsage(10, 3, 1)
	a.AccountID = "hidden"
	retry.add(a)
	a = newDiscardedAttemptUsage(20, 5, 1)
	a.AccountID = "visible"
	retry.add(a)
	retry.markLastVisibleContinuation()
	a = newDiscardedAttemptUsage(30, 7, 1)
	a.AccountID = "replayed"
	retry.add(a)
	retry.markLastContinuationAttemptOutput()
	s := retry.quotaSources()
	if s[0].Usage.CacheReadTokens != 13 || s[0].Usage.OutputTokens != 0 || s[1].Usage.CacheReadTokens != 20 || s[1].Usage.OutputTokens != 5 || s[2].Usage.CacheReadTokens != 37 || s[2].Usage.OutputTokens != 7 {
		t.Fatalf("wrong retry classification %+v", s)
	}
	input, cache := retry.apply(100, promptCacheUsage{})
	sources := quotaFinalSources(input, retry.applyOutput(17), cache, retry, quotaOwner{AccountID: "final", Input: 100, Output: 10})
	parts, err := allocateQuotaUsage(quotaRawUsage(input, 22, cache), sources)
	if err != nil || parts["hidden"].CacheReadTokens != 13 || parts["visible"].OutputTokens != 5 || parts["replayed"].OutputTokens != 7 || parts["final"].OutputTokens != 10 {
		t.Fatalf("parts=%+v err=%v", parts, err)
	}
}

func TestQuotaReceiptTTLAndFinalCap(t *testing.T) {
	setupBillingInflation(t, 50)
	cache := promptCacheUsage{CacheCreationInputTokens: 300, CacheCreation5mInputTokens: 100, CacheCreation1hInputTokens: 200, CacheReadInputTokens: 400}
	wire, billed := buildOpenAIUsageMapWithQuota(1000, 11, cache)
	u := quotaUsageFromClaude(billed)
	if u != (costledger.Usage{InputTokens: 450, OutputTokens: 17, CacheReadTokens: 600, CacheWrite5mTokens: 150, CacheWrite1hTokens: 300}) {
		t.Fatalf("usage=%+v", u)
	}
	if quotaInt(wire["prompt_tokens"]) != u.InputTokens+u.CacheReadTokens+u.CacheWrite5mTokens+u.CacheWrite1hTokens {
		t.Fatal("wire differs from canonical receipt")
	}
	response := &ResponsesObject{Usage: ResponsesUsage{InputTokens: 1000, OutputTokens: 11}}
	if got := quotaUsageFromClaude(applyResponsesPromptCacheUsage(response, cache)); got != u {
		t.Fatalf("Responses=%+v Chat=%+v", got, u)
	}
	ctx, r := withAccountQuotaReceipt(context.Background())
	stageAccountQuotaReceipt(ctx, "claude-opus-5-5", quotaRawUsage(1000, 11, cache), quotaFinalSources(1000, 11, cache, retryBillingUsage{}, quotaOwner{AccountID: "a", Input: 1000, Output: 11}))
	body := []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":300,\"output_tokens\":11,\"cache_creation_input_tokens\":300,\"cache_read_input_tokens\":400,\"kiro_context_usage\":{\"input_tokens\":900000}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	patched := patchClaudeCodeUsageTotals(body)
	patchAccountQuotaClaudeCodeReceipt(ctx, patched)
	actual := quotaClaudeSSETerminalUsage(patched)
	if r.usage.InputTokens != quotaInt(actual["input_tokens"]) || r.usage.OutputTokens != quotaInt(actual["output_tokens"]) || r.usage.CacheWrite5mTokens+r.usage.CacheWrite1hTokens != quotaInt(actual["cache_creation_input_tokens"]) {
		t.Fatalf("CC receipt=%+v actual=%+v", r.usage, actual)
	}
	total, _ := quotaPromptTokens(r.usage)
	if total != maxBillingInputTokens {
		t.Fatalf("CC cap=%d", total)
	}
	if _, err := allocateQuotaUsage(r.usage, r.sources); err != nil {
		t.Fatalf("CC corrected categories attribution: %v", err)
	}
}

func TestQuotaReceiptVisibleOnlyAfterAcceptedContent(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"ping", "event: ping\ndata: {\"type\":\"ping\"}\n\n", false},
		{"prelude", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n", false},
		{"error", "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"failed\"}}\n\n", false},
		{"role", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", false},
		{"text", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n", true},
		{"tool", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",\"name\":\"read\",\"input\":{}}}\n\n", true},
		{"chat", "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", true},
		{"responses", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n", true},
		{"short_text", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"par", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, r := withAccountQuotaReceipt(context.Background())
			for _, b := range []byte(tc.body) {
				r.observeWrite([]byte{b}, "text/event-stream")
			}
			if r.visible != tc.want {
				t.Fatalf("visible=%v want=%v", r.visible, tc.want)
			}
		})
	}
}

func TestQuotaReceiptSettlementSuccessAndFailures(t *testing.T) {
	setupBillingInflation(t, 0)
	for _, tc := range []struct {
		name                      string
		complete, visible, cancel bool
		writeErr                  error
		wantIncomplete            uint64
		wantUSD                   bool
	}{
		{"complete", true, true, false, nil, 0, true}, {"pure_error", false, false, false, nil, 0, false},
		{"only_prelude", false, false, false, nil, 0, false}, {"partial", false, true, false, nil, 1, false},
		{"write_failed", true, true, false, io.ErrClosedPipe, 1, false}, {"zero_bytes_failed", true, false, false, io.ErrClosedPipe, 0, false},
		{"cancelled", true, true, true, nil, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			l := attachQuotaTestLedger(t, h)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, r := withAccountQuotaReceipt(parent)
			noteAccountQuotaAttempt(ctx, "a", "claude-opus-5-5")
			u := costledger.Usage{InputTokens: 100, OutputTokens: 10}
			stageAccountQuotaReceipt(ctx, "claude-opus-5-5", u, []quotaSource{{AccountID: "a", Usage: u}})
			if tc.complete {
				completeAccountQuotaReceipt(ctx)
			}
			if tc.visible {
				r.observeWrite([]byte(`{"content":[{"type":"text","text":"ok"}]}`), "application/json")
			}
			if tc.cancel {
				cancel()
			}
			r.settle(h, tc.writeErr)
			r.settle(h, tc.writeErr)
			got := l.Snapshot("a", time.Now())
			if got.IncompleteRequests != tc.wantIncomplete || (got.SpentUSD > 0) != tc.wantUSD || got.UnpricedRequests != 0 {
				t.Fatalf("snapshot=%+v", got)
			}
		})
	}
}

func TestQuotaReceiptTierUsesFullRequestPrompt(t *testing.T) {
	setupBillingInflation(t, 0)
	h := &Handler{}
	l := attachQuotaTestLedger(t, h)
	ctx, r := withAccountQuotaReceipt(context.Background())
	u := costledger.Usage{InputTokens: 120000, OutputTokens: 100}
	stageAccountQuotaReceipt(ctx, "claude-haiku-5-5", u, []quotaSource{{AccountID: "a", Usage: costledger.Usage{InputTokens: 60000, OutputTokens: 50}}, {AccountID: "b", Usage: costledger.Usage{InputTokens: 60000, OutputTokens: 50}}})
	completeAccountQuotaReceipt(ctx)
	r.settle(h, nil)
	a, b := l.Snapshot("a", time.Now()), l.Snapshot("b", time.Now())
	want, err := costledger.DefaultCatalog().Quote("claude-haiku-5-5", u)
	if err != nil || math.Abs(a.SpentUSD+b.SpentUSD-want) > 1e-12 {
		t.Fatalf("a=%+v b=%+v full=%v err=%v", a, b, want, err)
	}
}

func TestQuotaReceiptHTTPProtocolsMatchFinalUsage(t *testing.T) {
	for _, tc := range []struct {
		name, path, extra string
		stream            bool
	}{
		{"claude_json", "/v1/messages", "", false}, {"claude_sse", "/v1/messages", "", true}, {"cc_sse", "/cc/v1/messages", "", true},
		{"structured_sse", "/v1/messages", "", true},
		{"chat_json", "/v1/chat/completions", "", false}, {"chat_sse", "/v1/chat/completions", `,"stream_options":{"include_usage":true}`, true}, {"chat_without_usage", "/v1/chat/completions", "", true},
		{"responses_json", "/v1/responses", "", false}, {"responses_sse", "/v1/responses", "", true},
	} {
		if tc.name == "structured_sse" {
			tc.path = "/v1/messages"
			tc.extra = `,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}}}`
		}
		t.Run(tc.name, func(t *testing.T) {
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, c *KiroStreamCallback) error {
				c.OnText(`{"answer":"ok"}`, false)
				if c.OnContextUsage != nil {
					c.OnContextUsage(90)
				}
				c.OnComplete(99999, 5)
				return nil
			})
			setBillingInflation(t, 50)
			attachQuotaTestLedger(t, h)
			input := `"messages":[{"role":"user","content":"hello"}]`
			if strings.Contains(tc.path, "responses") {
				input = `"input":"hello","store":false`
			}
			body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":128,"stream":%v,%s%s}`, tc.stream, input, tc.extra)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body)))
			if rec.Code != 200 {
				t.Fatalf("HTTP%d %s", rec.Code, rec.Body.String())
			}
			usd, unknown, incomplete := quotaLedgerTotal(h)
			if usd <= 0 || unknown != 0 || incomplete != 0 {
				t.Fatalf("USD=%v unknown=%d incomplete=%d body=%s", usd, unknown, incomplete, rec.Body.String())
			}
			var usage costledger.Usage
			if !tc.stream {
				var response map[string]interface{}
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				u := response["usage"].(map[string]interface{})
				if strings.Contains(tc.path, "messages") {
					usage = quotaUsageFromClaudeMap(u, promptCacheUsage{})
				} else if strings.Contains(tc.path, "responses") {
					usage = costledger.Usage{InputTokens: quotaInt(u["input_tokens"]), OutputTokens: quotaInt(u["output_tokens"])}
				} else {
					usage = costledger.Usage{InputTokens: quotaInt(u["prompt_tokens"]), OutputTokens: quotaInt(u["completion_tokens"])}
				}
			} else if strings.Contains(tc.path, "messages") {
				usage = quotaUsageFromClaudeMap(quotaClaudeSSETerminalUsage(rec.Body.Bytes()), promptCacheUsage{})
			} else {
				for _, frame := range splitQuotaSSE(rec.Body.Bytes()) {
					for _, line := range strings.Split(string(frame), "\n") {
						if !strings.HasPrefix(line, "data: {") {
							continue
						}
						var event map[string]interface{}
						if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
							continue
						}
						if u, ok := event["usage"].(map[string]interface{}); ok {
							usage = costledger.Usage{InputTokens: quotaInt(u["prompt_tokens"]), OutputTokens: quotaInt(u["completion_tokens"])}
						}
						if response, ok := event["response"].(map[string]interface{}); ok {
							if u, ok := response["usage"].(map[string]interface{}); ok {
								usage = costledger.Usage{InputTokens: quotaInt(u["input_tokens"]), OutputTokens: quotaInt(u["output_tokens"])}
							}
						}
					}
				}
			}
			if tc.name != "chat_without_usage" {
				want, err := costledger.DefaultCatalog().Quote("claude-opus-5-5", usage)
				if err != nil || math.Abs(usd-want) > 1e-12 {
					t.Fatalf("USD=%v wire=%+v want=%v err=%v", usd, usage, want, err)
				}
			}
		})
	}
}

func TestQuotaReceiptHTTPProtocolsCacheInflation(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/cc/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				var receipt *accountQuotaReceipt
				h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, p *KiroPayload, c *KiroStreamCallback) error {
					receipt = quotaReceiptFromContext(kiroPayloadContext(p))
					c.OnText("cached final answer", false)
					c.OnContextUsage(12.5)
					c.OnComplete(123, 4)
					return nil
				})
				t.Cleanup(h.stopPromptCachePolicyTrackers)
				attachQuotaTestLedger(t, h)
				setBillingInflation(t, 37.5)
				mode := config.CacheModeFixedRatio
				ratio := config.FixedRatioCacheConfig{CacheReadPercent: 50, CacheCreationPercent: 30}
				if err := config.UpdateCachePolicy(&mode, nil, &ratio); err != nil {
					t.Fatal(err)
				}
				prompt := strings.Repeat("stable public prompt for cache accounting ", 80)
				request := map[string]interface{}{"model": "claude-opus-5-5", "stream": stream, "max_tokens": 100}
				if path == "/v1/responses" {
					request["input"] = prompt
					request["store"] = false
				} else {
					request["messages"] = []map[string]string{{"role": "user", "content": prompt}}
				}
				if path == "/v1/chat/completions" && stream {
					request["stream_options"] = map[string]bool{"include_usage": true}
				}
				raw, _ := json.Marshal(request)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw))))
				if rec.Code != 200 || receipt == nil {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				u := receipt.usage
				if u.CacheReadTokens <= 0 || u.CacheWrite5mTokens <= 0 || u.CacheWrite1hTokens != 0 {
					t.Fatalf("missing cache categories %+v", u)
				}
				wire := billingProtocolUsage(t, rec.Body.String(), stream)
				if strings.Contains(path, "messages") {
					if quotaInt(wire["input_tokens"]) != u.InputTokens || quotaInt(wire["output_tokens"]) != u.OutputTokens || quotaInt(wire["cache_read_input_tokens"]) != u.CacheReadTokens || quotaInt(wire["cache_creation_input_tokens"]) != u.CacheWrite5mTokens+u.CacheWrite1hTokens {
						t.Fatalf("wire=%+v receipt=%+v", wire, u)
					}
				} else {
					input, output, details := "input_tokens", "output_tokens", "input_tokens_details"
					if strings.Contains(path, "chat") {
						input, output, details = "prompt_tokens", "completion_tokens", "prompt_tokens_details"
					}
					promptTokens, _ := quotaPromptTokens(u)
					detail := wire[details].(map[string]interface{})
					if quotaInt(wire[input]) != promptTokens || quotaInt(wire[output]) != u.OutputTokens || quotaInt(detail["cached_tokens"]) != u.CacheReadTokens {
						t.Fatalf("wire=%+v receipt=%+v", wire, u)
					}
				}
				want, err := costledger.DefaultCatalog().Quote("claude-opus-5-5", u)
				usd, unknown, incomplete := quotaLedgerTotal(h)
				if err != nil || math.Abs(usd-want) > 1e-12 || unknown != 0 || incomplete != 0 {
					t.Fatalf("USD=%v want=%v err=%v unknown=%d incomplete=%d", usd, want, err, unknown, incomplete)
				}
			})
		}
	}
}

func TestQuotaReceiptHTTPRetryAccountsSumToFinal(t *testing.T) {
	var calls int
	var accounts []string
	var h *Handler
	h = newEmptyVisibleOutputTestHandler(t, 2, func(a *config.Account, _ *KiroPayload, c *KiroStreamCallback) error {
		calls++
		accounts = append(accounts, a.ID)
		c.OnComplete(31, 7)
		if calls == 1 {
			c.OnText("discarded candidate", false)
			// The real transient retry policy prefers the same account. Make
			// this specific cross-owner fixture deterministic without changing it.
			h.pool.CooldownAccount(a.ID, time.Hour)
			return errors.New("upstream temporary disconnect")
		}
		c.OnText("final answer", false)
		return nil
	})
	attachQuotaTestLedger(t, h)
	setBillingInflation(t, 50)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`)))
	if rec.Code != 200 || calls != 2 || accounts[0] == accounts[1] {
		t.Fatalf("status=%d calls=%d ids=%v body=%s", rec.Code, calls, accounts, rec.Body.String())
	}
	var response ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	final := quotaUsageFromClaude(response.Usage)
	want, _ := costledger.DefaultCatalog().Quote("claude-opus-5-5", final)
	usd, unknown, incomplete := quotaLedgerTotal(h)
	if math.Abs(usd-want) > 1e-12 || unknown != 0 || incomplete != 0 {
		t.Fatalf("USD=%v want=%v unknown=%d incomplete=%d final=%+v", usd, want, unknown, incomplete, final)
	}
	if h.accountQuotaSnapshot(accounts[0]).SpentUSD <= 0 || h.accountQuotaSnapshot(accounts[1]).SpentUSD <= 0 {
		t.Fatal("retry or final owner was lost")
	}
}

func TestQuotaReceiptHTTPPureFailuresDoNotTaint(t *testing.T) {
	for _, err := range []error{errors.New("transport disconnected"), &KiroHTTPError{StatusCode: 403, Body: "account suspended"}} {
		t.Run(err.Error(), func(t *testing.T) {
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, _ *KiroStreamCallback) error { return err })
			attachQuotaTestLedger(t, h)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)))
			usd, unknown, incomplete := quotaLedgerTotal(h)
			if usd != 0 || unknown != 0 || incomplete != 0 {
				t.Fatalf("pure failure contaminated ledger USD=%v unpriced=%d partial=%d body=%s", usd, unknown, incomplete, rec.Body.String())
			}
		})
	}
}

func TestQuotaReceiptCCPrivateBufferIsNotDelivery(t *testing.T) {
	setupBillingInflation(t, 0)
	h := &Handler{}
	l := attachQuotaTestLedger(t, h)
	ctx, r := withAccountQuotaReceipt(context.Background())
	noteAccountQuotaAttempt(ctx, "a", "claude-opus-5-5")
	rec := httptest.NewRecorder()
	wrapped, bounded := wrapBoundedResponseWriterWithPolicy(rec, func() {}, nil, responseWritePolicy{})
	bounded.quotaReceipt = r
	private := &claudeCodeBufferedResponseWriter{destination: wrapped}
	private.Header().Set("Content-Type", "text/event-stream")
	_, _ = private.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"private text\"}}\n\n"))
	bounded.finish()
	r.settle(h, bounded.writeError())
	got := l.Snapshot("a", time.Now())
	if got.SpentUSD != 0 || got.IncompleteRequests != 0 {
		t.Fatalf("private CC buffer counted: %+v", got)
	}
}

func TestQuotaReceiptPartialOnlyMarksVisibleOwner(t *testing.T) {
	setupBillingInflation(t, 0)
	h := &Handler{}
	l := attachQuotaTestLedger(t, h)
	ctx, r := withAccountQuotaReceipt(context.Background())
	noteAccountQuotaAttempt(ctx, "failed-before-output", "claude-opus-5-5")
	r.observeWrite([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"), "text/event-stream")
	noteAccountQuotaAttempt(ctx, "partial-owner", "claude-opus-5-5")
	r.observeWrite([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial text\"}}\n\n"), "text/event-stream")
	r.settle(h, io.ErrUnexpectedEOF)
	if failed := l.Snapshot("failed-before-output", time.Now()); failed.IncompleteRequests != 0 || !failed.SpentComplete {
		t.Fatalf("pure failed attempt tainted %+v", failed)
	}
	if partial := l.Snapshot("partial-owner", time.Now()); partial.IncompleteRequests != 1 || partial.SpentComplete {
		t.Fatalf("partial owner lost %+v", partial)
	}
}

type quotaBlockingFlushWriter struct {
	header           http.Header
	entered, release chan struct{}
	once             sync.Once
}

func (w *quotaBlockingFlushWriter) Header() http.Header         { return w.header }
func (w *quotaBlockingFlushWriter) WriteHeader(int)             {}
func (w *quotaBlockingFlushWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *quotaBlockingFlushWriter) FlushError() error {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return io.ErrClosedPipe
}

func TestQuotaReceiptFinishWaitsForFailedInflightFlush(t *testing.T) {
	w := &quotaBlockingFlushWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	_, bounded := wrapBoundedResponseWriterWithPolicy(w, func() {}, nil, responseWritePolicy{})
	_, _ = bounded.Write([]byte("x"))
	go bounded.flush(true)
	<-w.entered
	done := make(chan struct{})
	go func() { bounded.finish(); close(done) }()
	select {
	case <-done:
		t.Fatal("finish did not wait for in-flight flush")
	case <-time.After(10 * time.Millisecond):
	}
	close(w.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("finish blocked")
	}
	if !errors.Is(bounded.writeError(), io.ErrClosedPipe) {
		t.Fatalf("flush error was not published before finish: %v", bounded.writeError())
	}
}
