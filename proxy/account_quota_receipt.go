package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"kiro-go/config"
	"kiro-go/costledger"
)

// A receipt belongs to one downstream request, not to an upstream attempt.
// Builders stage the exact final billing tuple; only the outer HTTP boundary
// commits it after the terminal entity and every write/flush have succeeded.
type accountQuotaReceiptKey struct{}

type quotaSource struct {
	AccountID    string
	Model        string // provenance only; the receipt's public model sets the price
	Usage        costledger.Usage
	InputWeight  int64
	OutputWeight int64
}

type quotaOwner struct {
	AccountID string
	Model     string
	Input     int64
	Output    int64
}

type quotaWebSearchDelivery struct {
	Context   context.Context
	AccountID string
}

func stageWebSearchQuotaReceipt(deliveries []quotaWebSearchDelivery, model string, input, output int, cache promptCacheUsage, usage map[string]interface{}) {
	if len(deliveries) == 0 || usage == nil {
		return
	}
	d := deliveries[0]
	stageAccountQuotaReceipt(d.Context, model, quotaUsageFromClaudeMap(usage, cache), []quotaSource{{AccountID: d.AccountID, Model: model, Usage: quotaRawUsage(input, output, cache)}})
}

func quotaClaudeSSETerminalUsage(body []byte) map[string]interface{} {
	var final map[string]interface{}
	for _, block := range splitQuotaSSE(body) {
		event, _, _, ok := decodeClaudeCodeSSEBlock(block)
		if ok && event["type"] == "message_delta" {
			final, _ = event["usage"].(map[string]interface{})
		}
	}
	return final
}

type accountQuotaReceipt struct {
	mu              sync.Mutex
	ctx             context.Context
	id              string
	model           string
	usage           costledger.Usage
	sources         []quotaSource
	attempts        map[string]string
	staged          bool
	complete        bool
	settled         bool
	invalid         bool
	visible         bool
	observePending  []byte
	visibleAccounts map[string]string
	activeOwners    []quotaOwner
}

func withAccountQuotaReceipt(ctx context.Context) (context.Context, *accountQuotaReceipt) {
	r := &accountQuotaReceipt{ctx: ctx, id: "quota_" + uuid.NewString(), attempts: map[string]string{}, visibleAccounts: map[string]string{}}
	return context.WithValue(ctx, accountQuotaReceiptKey{}, r), r
}

func quotaReceiptFromContext(ctx context.Context) *accountQuotaReceipt {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(accountQuotaReceiptKey{}).(*accountQuotaReceipt)
	return r
}

func noteAccountQuotaAttempt(ctx context.Context, accountID, model string) {
	r := quotaReceiptFromContext(ctx)
	if r == nil || accountID == "" {
		return
	}
	r.mu.Lock()
	r.attempts[accountID] = model
	r.activeOwners = []quotaOwner{{AccountID: accountID, Model: model}}
	r.mu.Unlock()
}

func withQuotaVisibleOwners(ctx context.Context, owners []quotaOwner) func() {
	r := quotaReceiptFromContext(ctx)
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	previous := append([]quotaOwner(nil), r.activeOwners...)
	r.activeOwners = append([]quotaOwner(nil), owners...)
	r.mu.Unlock()
	return func() { r.mu.Lock(); r.activeOwners = previous; r.mu.Unlock() }
}

func stageAccountQuotaReceipt(ctx context.Context, model string, usage costledger.Usage, sources []quotaSource) {
	r := quotaReceiptFromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	model, _ = ParseModelAndThinking(model, config.GetThinkingConfig().Suffix)
	r.model, r.usage = model, usage
	r.sources = append([]quotaSource(nil), sources...)
	r.staged = true
	for _, s := range sources {
		if s.AccountID != "" {
			r.attempts[s.AccountID] = model
		}
	}
}

func completeAccountQuotaReceipt(ctx context.Context) {
	if r := quotaReceiptFromContext(ctx); r != nil {
		r.mu.Lock()
		r.complete = true
		r.mu.Unlock()
	}
}

func invalidateQuotaReceiptWriter(w interface{}) {
	for w != nil {
		switch writer := w.(type) {
		case *boundedResponseWriter:
			if writer.quotaReceipt != nil {
				writer.quotaReceipt.mu.Lock()
				writer.quotaReceipt.invalid = true
				writer.quotaReceipt.mu.Unlock()
			}
			return
		case *boundedFlushingResponseWriter:
			w = writer.boundedResponseWriter
		case *claudeCodeBufferedResponseWriter:
			w = writer.destination
		case interface{ Unwrap() http.ResponseWriter }:
			w = writer.Unwrap()
		default:
			return
		}
	}
}

// CC's private buffer applies its context correction and inflation after the
// ordinary builder. Re-read only that final terminal usage; never re-inflate it.
func patchAccountQuotaClaudeCodeReceipt(ctx context.Context, body []byte) {
	r := quotaReceiptFromContext(ctx)
	if r == nil {
		return
	}
	final := quotaClaudeSSETerminalUsage(body)
	if final == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.staged {
		return
	}
	creation := quotaInt(final["cache_creation_input_tokens"])
	five, hour := quotaScalePair(r.usage.CacheWrite5mTokens, r.usage.CacheWrite1hTokens, creation)
	r.usage = costledger.Usage{InputTokens: quotaInt(final["input_tokens"]), OutputTokens: quotaInt(final["output_tokens"]), CacheReadTokens: quotaInt(final["cache_read_input_tokens"]), CacheWrite5mTokens: five, CacheWrite1hTokens: hour}
}

func splitQuotaSSE(body []byte) [][]byte {
	// The project's own SSE encoder emits LF frame boundaries.
	var frames [][]byte
	start := 0
	for i := 0; i+1 < len(body); i++ {
		if body[i] == '\n' && body[i+1] == '\n' {
			frames = append(frames, body[start:i])
			start = i + 2
			i++
		}
	}
	return frames
}

func (r *accountQuotaReceipt) settle(h *Handler, writeErr error) {
	if r == nil || h == nil {
		return
	}
	r.mu.Lock()
	if r.settled {
		r.mu.Unlock()
		return
	}
	r.settled = true
	model, id, usage := r.model, r.id, r.usage
	sources := append([]quotaSource(nil), r.sources...)
	attempts := make(map[string]string, len(r.attempts))
	for account, m := range r.attempts {
		attempts[account] = m
	}
	visibleAccounts := make(map[string]string, len(r.visibleAccounts))
	for account, m := range r.visibleAccounts {
		visibleAccounts[account] = m
	}
	staged := r.staged
	complete := staged && r.complete && !r.invalid && writeErr == nil && r.ctx.Err() == nil
	visible := r.visible
	r.mu.Unlock()
	if !complete {
		if !visible {
			return // pure errors/preludes and private CC buffers deliver no tokens
		}
		reason := "partial_delivery"
		if !staged {
			reason = "missing_usage"
		}
		if writeErr != nil {
			reason = "write_failed"
		} else if r.ctx.Err() != nil {
			reason = "cancelled"
		}
		for account, m := range visibleAccounts {
			if model != "" {
				m = model
			}
			h.recordAccountDollarUsageIncomplete(account, id, m, reason)
		}
		return
	}
	allocated, err := allocateQuotaUsage(usage, sources)
	promptTokens, promptErr := quotaPromptTokens(usage)
	if err == nil {
		err = promptErr
	}
	if err != nil {
		for account := range attempts {
			h.recordAccountDollarUsageIncomplete(account, id, model, "unknown_attribution")
		}
		return
	}
	for account, part := range allocated {
		h.recordAccountDollarUsageWithPromptTokens(account, id, model, part, promptTokens)
	}
}

func quotaPromptTokens(usage costledger.Usage) (int64, error) {
	var total int64
	for _, n := range []int64{usage.InputTokens, usage.CacheReadTokens, usage.CacheWrite5mTokens, usage.CacheWrite1hTokens} {
		if n < 0 || n > int64(^uint64(0)>>1)-total {
			return 0, costledger.ErrInvalidUsage
		}
		total += n
	}
	return total, nil
}

// Observe only bytes accepted by the OUTER destination, never a private CC
// buffer. A ping, role-only chunk, zero-usage prelude, or error is not delivery.
func (r *accountQuotaReceipt) observeWrite(p []byte, contentType string) {
	if r == nil || len(p) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	const observationLimit = 1 << 20
	room := observationLimit - len(r.observePending)
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		r.observePending = append(r.observePending, p[:room]...)
	}
	if !strings.Contains(contentType, "text/event-stream") {
		if !r.staged {
			return
		}
		var event map[string]interface{}
		if json.Unmarshal(r.observePending, &event) == nil {
			if quotaEventVisible(event) {
				r.markObservedVisible(true)
			}
		} else {
			if quotaPartialJSONVisible(r.observePending) {
				r.markObservedVisible(true)
			}
		}
		return
	}
	for {
		end := bytes.Index(r.observePending, []byte("\n\n"))
		if end < 0 {
			// A short write may stop inside a genuine content frame. Its
			// already accepted nonempty payload is still a partial delivery.
			if bytes.HasPrefix(r.observePending, []byte("event: content_block_delta\n")) || bytes.HasPrefix(r.observePending, []byte("event: response.output_text.delta\n")) || bytes.HasPrefix(r.observePending, []byte("data: {")) {
				if quotaPartialJSONVisible(r.observePending) {
					r.markObservedVisible(false)
				}
			}
			return
		}
		frame := r.observePending[:end]
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}
			var event map[string]interface{}
			if json.Unmarshal(line[len("data: "):], &event) == nil {
				if quotaEventVisible(event) {
					r.markObservedVisible(false)
				}
				if r.staged && quotaEventHasTerminalUsage(event) {
					r.markObservedVisible(true)
				}
			}
		}
		r.observePending = append(r.observePending[:0], r.observePending[end+2:]...)
	}
}

// Called with r.mu held. Only staged contributors are used for a final JSON
// body/terminal usage entity; old zero-output failed attempts are excluded.
func (r *accountQuotaReceipt) markObservedVisible(allSources bool) {
	r.visible = true
	if allSources && len(r.sources) > 0 {
		for _, s := range r.sources {
			if s.AccountID != "" {
				r.visibleAccounts[s.AccountID] = s.Model
			}
		}
		return
	}
	for _, o := range r.activeOwners {
		if o.AccountID != "" {
			r.visibleAccounts[o.AccountID] = o.Model
		}
	}
}

func quotaEventHasTerminalUsage(event map[string]interface{}) bool {
	if usage, ok := event["usage"].(map[string]interface{}); ok {
		for _, key := range []string{"input_tokens", "prompt_tokens", "output_tokens", "completion_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
			if quotaInt(usage[key]) > 0 {
				return true
			}
		}
	}
	if response, ok := event["response"].(map[string]interface{}); ok {
		return quotaEventHasTerminalUsage(response)
	}
	return false
}

func quotaPartialJSONVisible(p []byte) bool {
	for _, key := range []string{`"text":"`, `"text": "`, `"thinking":"`, `"content":"`, `"content": "`, `"partial_json":"`, `"delta":"`} {
		if at := bytes.Index(p, []byte(key)); at >= 0 {
			tail := p[at+len(key):]
			if len(tail) > 0 && tail[0] != '"' {
				return true
			}
		}
	}
	return false
}

func quotaEventVisible(event map[string]interface{}) bool {
	if content, ok := event["content"].(string); ok && content != "" {
		return true
	}
	for _, key := range []string{"content", "output", "tool_calls"} {
		if items, ok := event[key].([]interface{}); ok && len(items) > 0 {
			return true
		}
	}
	kind, _ := event["type"].(string)
	if kind == "content_block_start" {
		block, _ := event["content_block"].(map[string]interface{})
		if block["type"] == "tool_use" || block["type"] == "server_tool_use" || block["type"] == "redacted_thinking" {
			return true
		}
	}
	if delta, ok := event["delta"].(map[string]interface{}); ok {
		for _, key := range []string{"text", "thinking", "partial_json", "content", "reasoning_content", "reasoning"} {
			if s, _ := delta[key].(string); s != "" {
				return true
			}
		}
		if calls, ok := delta["tool_calls"].([]interface{}); ok && len(calls) > 0 {
			return true
		}
	}
	if strings.HasPrefix(kind, "response.") && strings.HasSuffix(kind, ".delta") {
		if s, _ := event["delta"].(string); s != "" {
			return true
		}
	}
	if item, ok := event["item"].(map[string]interface{}); ok {
		if item["type"] == "function_call" || quotaEventVisible(item) {
			return true
		}
	}
	if choices, ok := event["choices"].([]interface{}); ok {
		for _, value := range choices {
			if choice, ok := value.(map[string]interface{}); ok {
				if quotaEventVisible(choice) {
					return true
				}
				if message, ok := choice["message"].(map[string]interface{}); ok && quotaEventVisible(message) {
					return true
				}
			}
		}
	}
	return false
}

func quotaUsageFromClaude(usage ClaudeUsage) costledger.Usage {
	five, hour := int64(0), int64(0)
	if usage.CacheCreation != nil {
		five, hour = int64(usage.CacheCreation.Ephemeral5mInputTokens), int64(usage.CacheCreation.Ephemeral1hInputTokens)
	}
	five, hour = quotaScalePair(five, hour, int64(usage.CacheCreationInputTokens))
	return costledger.Usage{InputTokens: int64(usage.InputTokens), OutputTokens: int64(usage.OutputTokens), CacheReadTokens: int64(usage.CacheReadInputTokens), CacheWrite5mTokens: five, CacheWrite1hTokens: hour}
}

func quotaUsageFromClaudeMap(usage map[string]interface{}, cache promptCacheUsage) costledger.Usage {
	creation := quotaInt(usage["cache_creation_input_tokens"])
	five, hour := quotaScalePair(int64(cache.CacheCreation5mInputTokens), int64(cache.CacheCreation1hInputTokens), creation)
	return costledger.Usage{InputTokens: quotaInt(usage["input_tokens"]), OutputTokens: quotaInt(usage["output_tokens"]), CacheReadTokens: quotaInt(usage["cache_read_input_tokens"]), CacheWrite5mTokens: five, CacheWrite1hTokens: hour}
}

func quotaInt(value interface{}) int64 {
	switch n := value.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		if n >= 0 && n < 9223372036854775808.0 {
			return int64(n)
		}
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func quotaScalePair(five, hour, total int64) (int64, int64) {
	if total <= 0 {
		return 0, 0
	}
	if five <= 0 && hour <= 0 {
		return total, 0 // existing cache tracker treats unspecified writes as 5m
	}
	f, h := scaleClaudeCodeCacheCreation(int(five), int(hour), int(total))
	return int64(f), int64(h)
}

func quotaClaudeOwners(accountID, model string, input, output int, prelude *claudeStreamPrelude) []quotaOwner {
	owners := []quotaOwner{{AccountID: accountID, Model: model, Input: int64(input), Output: int64(output)}}
	if prelude != nil {
		owners = append(owners, prelude.QuotaOwners...)
	}
	return owners
}

func updateAccountQuotaClaudeMap(ctx context.Context, usage map[string]interface{}, cache promptCacheUsage) {
	if r := quotaReceiptFromContext(ctx); r != nil {
		r.mu.Lock()
		r.usage = quotaUsageFromClaudeMap(usage, cache)
		r.mu.Unlock()
	}
}

func updateAccountQuotaClaudeUsage(ctx context.Context, usage ClaudeUsage) {
	if r := quotaReceiptFromContext(ctx); r != nil {
		r.mu.Lock()
		r.usage = quotaUsageFromClaude(usage)
		r.mu.Unlock()
	}
}

func quotaRawUsage(input, output int, cache promptCacheUsage) costledger.Usage {
	five, hour := quotaScalePair(int64(cache.CacheCreation5mInputTokens), int64(cache.CacheCreation1hInputTokens), int64(cache.CacheCreationInputTokens))
	return costledger.Usage{InputTokens: int64(billedClaudeInputTokens(input, cache)), OutputTokens: int64(output), CacheReadTokens: int64(cache.CacheReadInputTokens), CacheWrite5mTokens: five, CacheWrite1hTokens: hour}
}

func maxQuota(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func quotaCounts(u costledger.Usage) []int64 {
	return []int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWrite5mTokens, u.CacheWrite1hTokens}
}

func quotaUsageCounts(v []int64) costledger.Usage {
	return costledger.Usage{InputTokens: v[0], OutputTokens: v[1], CacheReadTokens: v[2], CacheWrite5mTokens: v[3], CacheWrite1hTokens: v[4]}
}

// Arbitrary precision products prevent integer overflow, while deterministic
// largest remainders conserve EVERY final token through caps and inflation.
func quotaLargestRemainder(total int64, weights []int64) ([]int64, error) {
	result := make([]int64, len(weights))
	if total < 0 {
		return nil, costledger.ErrInvalidUsage
	}
	sum := new(big.Int)
	for _, w := range weights {
		if w < 0 {
			return nil, costledger.ErrInvalidUsage
		}
		sum.Add(sum, big.NewInt(w))
	}
	if total == 0 {
		return result, nil
	}
	if sum.Sign() == 0 {
		return nil, errors.New("positive final usage has no attributable source")
	}
	type remainder struct {
		index int
		value *big.Int
	}
	remainders := make([]remainder, len(weights))
	remaining := total
	for i, w := range weights {
		product := new(big.Int).Mul(big.NewInt(total), big.NewInt(w))
		q, rem := new(big.Int), new(big.Int)
		q.QuoRem(product, sum, rem)
		result[i] = q.Int64()
		remaining -= result[i]
		remainders[i] = remainder{i, rem}
	}
	sort.SliceStable(remainders, func(i, j int) bool { return remainders[i].value.Cmp(remainders[j].value) > 0 })
	for i := int64(0); i < remaining; i++ {
		result[remainders[i].index]++
	}
	return result, nil
}

func allocateQuotaUsage(final costledger.Usage, sources []quotaSource) (map[string]costledger.Usage, error) {
	// Merge first: two rounds on the same account must consume one receipt ID.
	byAccount := map[string][]int64{}
	inputWeights, outputWeights := map[string]int64{}, map[string]int64{}
	for _, source := range sources {
		counts := quotaCounts(source.Usage)
		if source.AccountID == "" {
			for _, n := range counts {
				if n != 0 {
					return nil, errors.New("missing account provenance")
				}
			}
			continue
		}
		if byAccount[source.AccountID] == nil {
			byAccount[source.AccountID] = make([]int64, 5)
		}
		for i, n := range counts {
			if n < 0 || n > int64(^uint64(0)>>1)-byAccount[source.AccountID][i] {
				return nil, costledger.ErrInvalidUsage
			}
			byAccount[source.AccountID][i] += n
		}
		for _, pair := range []struct {
			value  int64
			target map[string]int64
		}{{source.InputWeight, inputWeights}, {source.OutputWeight, outputWeights}} {
			if pair.value < 0 || pair.value > int64(^uint64(0)>>1)-pair.target[source.AccountID] {
				return nil, costledger.ErrInvalidUsage
			}
			pair.target[source.AccountID] += pair.value
		}
	}
	accounts := make([]string, 0, len(byAccount))
	for account := range byAccount {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	parts := make([][]int64, len(accounts))
	for i := range parts {
		parts[i] = make([]int64, 5)
	}
	for category, total := range quotaCounts(final) {
		weights := make([]int64, len(accounts))
		hasWeight := false
		for i, account := range accounts {
			weights[i] = byAccount[account][category]
			hasWeight = hasWeight || weights[i] > 0
		}
		if !hasWeight && total > 0 {
			for i, account := range accounts {
				weights[i] = inputWeights[account]
				if category == 1 {
					weights[i] = outputWeights[account]
				}
			}
		}
		allocated, err := quotaLargestRemainder(total, weights)
		if err != nil {
			return nil, err
		}
		for i := range parts {
			parts[i][category] = allocated[i]
		}
	}
	result := map[string]costledger.Usage{}
	for i, account := range accounts {
		result[account] = quotaUsageCounts(parts[i])
	}
	return result, nil
}

// quotaFinalSources distributes the non-retry public tuple by its real model
// call owners. Retry usage has its own account and public category provenance.
// This allocates the published total; it never adds hidden upstream cost again.
func quotaFinalSources(input, output int, cache promptCacheUsage, retry retryBillingUsage, owners ...quotaOwner) []quotaSource {
	retries := retry.quotaSources()
	base := []int64{int64(billedClaudeInputTokens(input, cache)), int64(output), int64(cache.CacheReadInputTokens), int64(cache.CacheCreation5mInputTokens), int64(cache.CacheCreation1hInputTokens)}
	if cache.CacheCreationInputTokens > 0 && base[3]+base[4] == 0 {
		base[3] = int64(cache.CacheCreationInputTokens)
	}
	for _, source := range retries {
		for i, n := range quotaCounts(source.Usage) {
			base[i] = maxQuota(0, base[i]-n)
		}
	}
	result := append([]quotaSource(nil), retries...)
	for _, owner := range owners {
		result = append(result, quotaSource{AccountID: owner.AccountID, Model: owner.Model, InputWeight: maxQuota(0, owner.Input), OutputWeight: maxQuota(0, owner.Output)})
	}
	for category, total := range base {
		weights := make([]int64, len(owners))
		for i, owner := range owners {
			weights[i] = maxQuota(0, owner.Input)
			if category == 1 {
				weights[i] = maxQuota(0, owner.Output)
			}
		}
		allocated, err := quotaLargestRemainder(total, weights)
		if err != nil {
			// Keep orphan usage visible to validation instead of assigning it to
			// whichever account happens to be last in the loop.
			orphan := make([]int64, 5)
			orphan[category] = total
			result = append(result, quotaSource{Usage: quotaUsageCounts(orphan)})
			continue
		}
		for i, n := range allocated {
			counts := quotaCounts(result[len(retries)+i].Usage)
			counts[category] = n
			result[len(retries)+i].Usage = quotaUsageCounts(counts)
		}
	}
	return result
}
