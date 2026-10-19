package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/costledger"
)

func assertQuotaRouteReceipt(t *testing.T, h *Handler, r *accountQuotaReceipt, rec *httptest.ResponseRecorder, path string, stream bool) float64 {
	t.Helper()
	if rec.Code != http.StatusOK || r == nil || !r.complete || !r.settled {
		t.Fatalf("status=%d receipt=%+v body=%s", rec.Code, r, rec.Body.String())
	}
	u := r.usage
	wire := billingProtocolUsage(t, rec.Body.String(), stream)
	if strings.Contains(path, "messages") {
		if quotaInt(wire["input_tokens"]) != u.InputTokens || quotaInt(wire["output_tokens"]) != u.OutputTokens || quotaInt(wire["cache_creation_input_tokens"]) != u.CacheWrite5mTokens+u.CacheWrite1hTokens || quotaInt(wire["cache_read_input_tokens"]) != u.CacheReadTokens {
			t.Fatalf("receipt=%+v wire=%+v", u, wire)
		}
	} else {
		input, output, details := "input_tokens", "output_tokens", "input_tokens_details"
		if strings.Contains(path, "chat") {
			input, output, details = "prompt_tokens", "completion_tokens", "prompt_tokens_details"
		}
		total, _ := quotaPromptTokens(u)
		if quotaInt(wire[input]) != total || quotaInt(wire[output]) != u.OutputTokens {
			t.Fatalf("receipt=%+v wire=%+v", u, wire)
		}
		if d, ok := wire[details].(map[string]interface{}); ok && quotaInt(d["cached_tokens"]) != u.CacheReadTokens {
			t.Fatalf("cached receipt=%+v wire=%+v", u, wire)
		}
	}
	want, err := costledger.DefaultCatalog().Quote(r.model, u)
	if err != nil {
		t.Fatal(err)
	}
	return want
}

func TestQuotaReceiptHTTPAdvisorOwnership(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var h *Handler
			var receipt *accountQuotaReceipt
			var ids []string
			h = newEmptyVisibleOutputTestHandler(t, 2, func(a *config.Account, p *KiroPayload, c *KiroStreamCallback) error {
				receipt = quotaReceiptFromContext(kiroPayloadContext(p))
				ids = append(ids, a.ID)
				if len(ids) == 1 {
					c.OnText("Check the state first.", false)
					h.pool.CooldownAccount(a.ID, time.Hour)
				} else {
					c.OnText("State checked; final answer.", false)
				}
				c.OnComplete(10, 5)
				return nil
			})
			attachQuotaTestLedger(t, h)
			setBillingInflation(t, 25)
			body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":256,"stream":%v,"messages":[{"role":"user","content":"Inspect this state."}],"tools":[{"type":"advisor_20260301","name":"advisor","model":"claude-opus-4-8"}]}`, stream)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			want := assertQuotaRouteReceipt(t, h, receipt, rec, "/v1/messages", stream)
			usd, unknown, incomplete := quotaLedgerTotal(h)
			if len(ids) != 2 || ids[0] == ids[1] || receipt.model != "claude-sonnet-4.6" || math.Abs(usd-want) > 1e-12 || unknown != 0 || incomplete != 0 {
				t.Fatalf("ids=%v model=%s USD=%v want=%v unknown=%d partial=%d", ids, receipt.model, usd, want, unknown, incomplete)
			}
			for _, id := range ids {
				if h.accountQuotaSnapshot(id).SpentUSD <= 0 {
					t.Fatalf("owner %s lost", id)
				}
			}
		})
	}
}

func TestQuotaReceiptHTTPCodeExecutionMultiRoundOwnership(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var h *Handler
			var receipt *accountQuotaReceipt
			var ids []string
			var executions int
			h = newEmptyVisibleOutputTestHandler(t, 2, func(a *config.Account, p *KiroPayload, c *KiroStreamCallback) error {
				receipt = quotaReceiptFromContext(kiroPayloadContext(p))
				ids = append(ids, a.ID)
				if len(ids) == 1 {
					c.OnToolUse(KiroToolUse{ToolUseID: "quota-exec", Name: codeExecutionToolName, Input: map[string]interface{}{"code": "print(2+2)"}})
					c.OnStopReason("TOOL_USE")
					h.pool.CooldownAccount(a.ID, time.Hour)
				} else {
					c.OnText("The result is 4.", false)
					c.OnStopReason("END_TURN")
				}
				c.OnComplete(9, 6)
				return nil
			})
			h.codeExecutor = codeExecutorFunc(func(context.Context, string) (CodeExecutionResult, error) {
				executions++
				return CodeExecutionResult{Stdout: "4\n", ReturnCode: 0}, nil
			})
			attachQuotaTestLedger(t, h)
			setBillingInflation(t, 40)
			body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":256,"stream":%v,"messages":[{"role":"user","content":"Run a calculation."}],"tools":[{"type":"code_execution_20250522","name":"code_execution"}]}`, stream)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			want := assertQuotaRouteReceipt(t, h, receipt, rec, "/v1/messages", stream)
			usd, unknown, incomplete := quotaLedgerTotal(h)
			if executions != 1 || len(ids) != 2 || ids[0] == ids[1] || math.Abs(usd-want) > 1e-12 || unknown != 0 || incomplete != 0 {
				t.Fatalf("ids=%v exec=%d USD=%v want=%v unknown=%d partial=%d body=%s", ids, executions, usd, want, unknown, incomplete, rec.Body.String())
			}
			for _, id := range ids {
				if h.accountQuotaSnapshot(id).SpentUSD <= 0 {
					t.Fatalf("round owner %s lost", id)
				}
			}
		})
	}
}

func TestQuotaReceiptHTTPWebSearch(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", path, stream), func(t *testing.T) {
				h := newEmptyVisibleOutputTestHandler(t, 1, func(*config.Account, *KiroPayload, *KiroStreamCallback) error {
					t.Fatal("websearch used generation path")
					return nil
				})
				var receipt *accountQuotaReceipt
				var calls int
				h.webSearchMCPCaller = func(ctx context.Context, _ *config.Account, _ string) (*webSearchResultSet, error) {
					receipt = quotaReceiptFromContext(ctx)
					calls++
					return testWebSearchResultSet(), nil
				}
				attachQuotaTestLedger(t, h)
				setBillingInflation(t, 50)
				body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":256,"stream":%v,"messages":[{"role":"user","content":"Latest release"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, stream)
				if path == "/v1/responses" {
					body = fmt.Sprintf(`{"model":"claude-opus-5-5","stream":%v,"input":"Latest release","tools":[{"type":"web_search_preview"}],"store":false}`, stream)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
				want := assertQuotaRouteReceipt(t, h, receipt, rec, path, stream)
				usd, unknown, incomplete := quotaLedgerTotal(h)
				if calls != 1 || math.Abs(usd-want) > 1e-12 || unknown != 0 || incomplete != 0 {
					t.Fatalf("calls=%d USD=%v want=%v unknown=%d partial=%d", calls, usd, want, unknown, incomplete)
				}
			})
		}
	}
}

func TestQuotaReceiptHTTPFunctionCacheMissThenHit(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			var receipt *accountQuotaReceipt
			var calls int
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, p *KiroPayload, c *KiroStreamCallback) error {
				receipt = quotaReceiptFromContext(kiroPayloadContext(p))
				calls++
				c.OnText("fresh upstream output", false)
				c.OnComplete(10, 5)
				return nil
			})
			t.Cleanup(h.stopPromptCachePolicyTrackers)
			attachQuotaTestLedger(t, h)
			setBillingInflation(t, 50)
			mode := config.CacheModeFunctionSimulation
			hit, maxRead := 100, 85
			fc := config.FunctionCacheConfig{Enabled: true, SimulatedTokens: 200, TTLSeconds: 3600, HitRatePercent: &hit, MaxCacheReadPercent: &maxRead}
			if err := config.UpdateCachePolicy(&mode, &fc, nil); err != nil {
				t.Fatal(err)
			}
			request := map[string]interface{}{"model": "claude-opus-5-5", "max_tokens": 256}
			prompt := strings.Repeat("stable self managed cache prompt ", 100)
			if path == "/v1/responses" {
				request["input"] = prompt
				request["store"] = false
			} else {
				request["messages"] = []map[string]string{{"role": "user", "content": prompt}}
			}
			raw, _ := json.Marshal(request)
			var expected float64
			for n := 0; n < 2; n++ {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw))))
				expected += assertQuotaRouteReceipt(t, h, receipt, rec, path, false)
				if n == 0 && (receipt.usage.CacheWrite1hTokens <= 0 || receipt.usage.CacheReadTokens != 0) {
					t.Fatalf("miss=%+v", receipt.usage)
				}
				if n == 1 && (receipt.usage.CacheReadTokens <= 0 || receipt.usage.CacheWrite1hTokens != 0 || receipt.usage.CacheWrite5mTokens != 0) {
					t.Fatalf("hit=%+v", receipt.usage)
				}
				usd, unknown, incomplete := quotaLedgerTotal(h)
				if math.Abs(usd-expected) > 1e-12 || unknown != 0 || incomplete != 0 {
					t.Fatalf("USD=%v expected=%v unknown=%d partial=%d", usd, expected, unknown, incomplete)
				}
			}
			if calls != 2 {
				t.Fatal("simulation incorrectly skipped real generation")
			}
		})
	}
}

func TestQuotaReceiptHTTPAliasesAreExplicitlyPriced(t *testing.T) {
	for _, alias := range []string{"gpt-4", "anthropic/claude-opus-5-5-thinking"} {
		t.Run(alias, func(t *testing.T) {
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, c *KiroStreamCallback) error {
				c.OnText("answer", false)
				c.OnComplete(5, 2)
				return nil
			})
			attachQuotaTestLedger(t, h)
			body, _ := json.Marshal(map[string]interface{}{"model": alias, "messages": []map[string]string{{"role": "user", "content": "hello"}}})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))))
			usd, unknown, incomplete := quotaLedgerTotal(h)
			if rec.Code != 200 || usd <= 0 || unknown != 0 || incomplete != 0 {
				t.Fatalf("alias=%s status=%d usd=%v unknown=%d partial=%d body=%s", alias, rec.Code, usd, unknown, incomplete, rec.Body.String())
			}
		})
	}
}
