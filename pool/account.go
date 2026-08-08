// Package pool 账号池管理
// 实现轮询负载均衡、错误冷却、Token 刷新、账号级并发控制
package pool

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kiro-go/config"
)

const (
	tokenRefreshSkewSeconds int64 = 120
	statsPersistAttempts          = 3
)

type accountStatsUpdate struct {
	id           string
	requestCount int
	errorCount   int
	totalTokens  int
	totalCredits float64
	lastUsed     int64
}

// RuntimeAccountState describes whether an account is currently part of the
// dispatch pool and can accept a request. It intentionally mirrors the
// aggregate AvailableCount criteria without model-specific routing.
type RuntimeAccountState struct {
	Available bool
	Reason    string
}

// AccountPool 账号池
type AccountPool struct {
	reloadMu       sync.Mutex // preserves Reload caller order while config is read outside mu
	mu             sync.RWMutex
	accounts       []config.Account
	uniqueIndexes  []int // first weighted-list index for each account ID, rebuilt by Reload
	accountIndexes map[string][]int
	totalAccounts  int
	currentIndex   uint64
	cooldowns      map[string]time.Time       // 账号冷却时间
	errorCounts    map[string]int             // 连续错误计数
	modelLists     map[string]map[string]bool // accountID → set of modelIDs (from ListAvailableModels)
	inFlight       map[string]int             // accountID → 当前并发占用数
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:   make(map[string]time.Time),
			errorCounts: make(map[string]int),
			modelLists:  make(map[string]map[string]bool),
			inFlight:    make(map[string]int),
		}
		pool.Reload()
	})
	return pool
}

// Reload rebuilds the weighted account list from config.
// Weight <= 1 → 1 entry; weight >= 2 → weight entries.
// Over-quota accounts are dropped unless either the per-account upstream
// Overages switch (OverageStatus=ENABLED) or the global AllowOverUsage
// setting permits over-quota routing.
func (p *AccountPool) Reload() {
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()
	enabled := config.GetEnabledAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Config may not yet contain counters accumulated since the last 30-second
	// flush. Preserve the newer in-memory values across policy/account reloads.
	liveStats := make(map[string]config.AccountStatsSnapshot)
	for i := range p.accounts {
		account := p.accounts[i]
		if _, exists := liveStats[account.ID]; exists {
			continue
		}
		liveStats[account.ID] = config.AccountStatsSnapshot{
			RequestCount: account.RequestCount,
			ErrorCount:   account.ErrorCount,
			TotalTokens:  account.TotalTokens,
			TotalCredits: account.TotalCredits,
			LastUsed:     account.LastUsed,
		}
	}
	var weighted []config.Account
	uniqueIndexes := make([]int, 0, len(enabled))
	uniqueIDs := make(map[string]struct{}, len(enabled))
	accountIndexes := make(map[string][]int, len(enabled))
	for _, a := range enabled {
		if stats, ok := liveStats[a.ID]; ok {
			a.RequestCount = stats.RequestCount
			a.ErrorCount = stats.ErrorCount
			a.TotalTokens = stats.TotalTokens
			a.TotalCredits = stats.TotalCredits
			a.LastUsed = stats.LastUsed
		}
		if isQuotaBlocked(a, allowOverUsage) {
			continue
		}
		if _, duplicate := uniqueIDs[a.ID]; !duplicate {
			uniqueIDs[a.ID] = struct{}{}
			uniqueIndexes = append(uniqueIndexes, len(weighted))
		}
		w := effectiveWeight(a.Weight)
		for j := 0; j < w; j++ {
			accountIndexes[a.ID] = append(accountIndexes[a.ID], len(weighted))
			weighted = append(weighted, a)
		}
	}
	p.accounts = weighted
	p.uniqueIndexes = uniqueIndexes
	p.accountIndexes = accountIndexes
	p.totalAccounts = len(enabled)
	if p.inFlight == nil {
		p.inFlight = make(map[string]int)
	}
	active := make(map[string]bool)
	for _, a := range enabled {
		active[a.ID] = true
	}
	for id := range p.inFlight {
		if !active[id] {
			delete(p.inFlight, id)
		}
	}
}

// GetNext 获取下一个可用账号（不占用并发，仅用于兼容测试/旧调用）
func (p *AccountPool) GetNext() *config.Account {
	return p.GetNextExcluding(nil)
}

// GetNextExcluding 获取下一个可用账号（不占用并发），并跳过指定账号。
func (p *AccountPool) GetNextExcluding(excluded map[string]bool) *config.Account {
	mode := config.GetSchedulingConfig().Mode
	selection := loadAccountSelectionPolicy("")
	p.mu.RLock()
	defer p.mu.RUnlock()
	acc, _ := p.selectNextLocked("", excluded, false, mode, selection)
	return copyAccount(acc)
}

// AcquireNextForModelExcluding selects a usable account for the model and increments
// its in-flight counter atomically. The bool is true when there are otherwise usable
// accounts, but every one of them has reached its configured concurrency limit.
func (p *AccountPool) AcquireNextForModelExcluding(model string, excluded map[string]bool) (*config.Account, bool) {
	mode := config.GetSchedulingConfig().Mode
	selection := loadAccountSelectionPolicy(model)
	p.mu.Lock()
	defer p.mu.Unlock()
	acc, allFull := p.selectNextLocked(model, excluded, true, mode, selection)
	if acc != nil {
		p.inFlight[acc.ID]++
	}
	return copyAccount(acc), allFull
}

// AcquireByIDForModel tries to acquire a specific account, respecting model support,
// cooldown, quota, token freshness, exclusion, and per-account concurrency.
func (p *AccountPool) AcquireByIDForModel(id, model string, excluded map[string]bool) (*config.Account, bool) {
	selection := loadAccountSelectionPolicy(model)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	acc := p.accountByIDLocked(id)
	if !p.isSelectableLocked(acc, model, excluded, now, selection) {
		return nil, false
	}
	if p.isConcurrencyFullLocked(acc) {
		return nil, true
	}
	p.inFlight[acc.ID]++
	return copyAccount(acc), false
}

// AcquireForTest leases a specific configured account for an explicit admin
// test. It intentionally bypasses routing cooldowns, quota gates, and cached
// model filters so an operator can verify the account itself. The concurrency
// limit remains enforced to avoid disrupting live traffic.
func (p *AccountPool) AcquireForTest(account *config.Account) (*config.Account, bool) {
	if account == nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isConcurrencyFullLocked(account) {
		return nil, true
	}
	p.inFlight[account.ID]++
	return copyAccount(account), false
}

// Release decrements the in-flight counter for an account acquired by Acquire*.
func (p *AccountPool) Release(id string) {
	if id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight[id] <= 1 {
		delete(p.inFlight, id)
		return
	}
	p.inFlight[id]--
}

// InFlight returns the current in-flight request count for an account.
func (p *AccountPool) InFlight(id string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.inFlight[id]
}

// UpdateProfileArn updates every weighted copy of an account after first-use
// discovery so subsequent leases do not repeat the cross-region lookup.
func (p *AccountPool) UpdateProfileArn(id, profileArn string) {
	if p == nil || id == "" || strings.TrimSpace(profileArn) == "" {
		return
	}
	profileArn = strings.TrimSpace(profileArn)
	p.mu.Lock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].ProfileArn = profileArn
		}
	}
	p.mu.Unlock()
}

// AvailabilitySnapshot describes why the scheduler can or cannot select accounts
// for a request. It is intentionally read-only and used by handlers to distinguish
// "no accounts configured" from "accounts are temporarily refreshing tokens".
type AvailabilitySnapshot struct {
	Selectable      int
	Full            int
	Expired         int
	ExpiredAccounts []config.Account
}

func tokenNeedsRefresh(acc *config.Account, now time.Time) bool {
	return acc != nil && acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds
}

// InspectAvailability reports accounts that are otherwise usable for model but
// skipped because their access token is expired/near expiry. The checks mirror
// isSelectableLocked, except token freshness is returned separately.
func (p *AccountPool) InspectAvailability(model string, excluded map[string]bool) AvailabilitySnapshot {
	selection := loadAccountSelectionPolicy(model)
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	snap := AvailabilitySnapshot{}

	for _, accountIndex := range p.uniqueAccountIndexesLocked() {
		acc := &p.accounts[accountIndex]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if model != "" && !p.accountHasModel(acc.ID, model) {
			continue
		}
		if !selection.accountAllowed(acc) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		if isQuotaBlocked(*acc, selection.allowOverUsage) {
			continue
		}
		if tokenNeedsRefresh(acc, now) {
			snap.Expired++
			snap.ExpiredAccounts = append(snap.ExpiredAccounts, *acc)
			continue
		}
		if p.isConcurrencyFullLocked(acc) {
			snap.Full++
			continue
		}
		snap.Selectable++
	}
	return snap
}

type accountSelectionPolicy struct {
	allowOverUsage      bool
	restrictLevels      bool
	allowedAccountLevel map[string]struct{}
}

// loadAccountSelectionPolicy snapshots config before the pool lock is acquired.
// Priority rules are normalized by config, so the first matching rule can be
// reduced once per request to an account-level membership check.
func loadAccountSelectionPolicy(model string) accountSelectionPolicy {
	selection := accountSelectionPolicy{allowOverUsage: config.GetAllowOverUsage()}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return selection
	}
	priority := config.GetAccountPriorityConfig()
	if !priority.Enabled {
		return selection
	}
	for _, rule := range priority.Rules {
		if !rule.Enabled {
			continue
		}
		matched := false
		for _, candidate := range rule.Models {
			if candidate == "*" || candidate == model {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		selection.restrictLevels = true
		selection.allowedAccountLevel = make(map[string]struct{}, len(rule.AccountLevels))
		for _, level := range rule.AccountLevels {
			selection.allowedAccountLevel[level] = struct{}{}
		}
		break
	}
	return selection
}

func (selection accountSelectionPolicy) accountAllowed(account *config.Account) bool {
	if !selection.restrictLevels {
		return true
	}
	if account == nil {
		return false
	}
	_, allowed := selection.allowedAccountLevel[config.EffectiveAccountLevel(*account)]
	return allowed
}

func (p *AccountPool) selectNextLocked(model string, excluded map[string]bool, respectConcurrency bool, mode string, selection accountSelectionPolicy) (*config.Account, bool) {
	if len(p.accounts) == 0 {
		return nil, false
	}
	switch mode {
	case config.SchedulingModeRoundRobin:
		return p.selectRoundRobinLocked(model, excluded, respectConcurrency, selection)
	case config.SchedulingModeLeastRequests:
		return p.selectBestAccountLocked(model, excluded, respectConcurrency, mode, selection)
	case config.SchedulingModeLeastCredits:
		return p.selectBestAccountLocked(model, excluded, respectConcurrency, mode, selection)
	case config.SchedulingModeLoadBalance:
		return p.selectBestAccountLocked(model, excluded, respectConcurrency, mode, selection)
	default:
		return p.selectBestAccountLocked(model, excluded, respectConcurrency, config.SchedulingModeLoadBalance, selection)
	}
}

func (p *AccountPool) selectRoundRobinLocked(model string, excluded map[string]bool, respectConcurrency bool, selection accountSelectionPolicy) (*config.Account, bool) {
	now := time.Now()
	n := len(p.accounts)
	eligible := 0
	full := 0

	// Do not de-duplicate IDs here: p.accounts is already a weighted list, so
	// duplicate entries are the intended way weight influences round-robin order.
	for i := 0; i < n; i++ {
		idx := (atomic.AddUint64(&p.currentIndex, 1) - 1) % uint64(n)
		acc := &p.accounts[idx]
		if !p.isSelectableLocked(acc, model, excluded, now, selection) {
			continue
		}
		eligible++
		if respectConcurrency && p.isConcurrencyFullLocked(acc) {
			full++
			continue
		}
		return acc, false
	}
	return nil, eligible > 0 && full == eligible
}

func (p *AccountPool) selectBestAccountLocked(model string, excluded map[string]bool, respectConcurrency bool, mode string, selection accountSelectionPolicy) (*config.Account, bool) {
	now := time.Now()
	var best *config.Account
	eligible := 0
	full := 0

	for _, accountIndex := range p.uniqueAccountIndexesLocked() {
		acc := &p.accounts[accountIndex]
		if !p.isSelectableLocked(acc, model, excluded, now, selection) {
			continue
		}
		eligible++
		if respectConcurrency && p.isConcurrencyFullLocked(acc) {
			full++
			continue
		}
		if best == nil || p.accountRanksBeforeLocked(acc, best, mode) {
			best = acc
		}
	}

	if best == nil {
		return nil, eligible > 0 && full == eligible
	}
	return best, false
}

// uniqueAccountIndexesLocked returns the Reload-built unique index without an
// allocation in production. The fallback keeps directly-constructed pools used
// by tests and embedders compatible without making the request path maintain a
// second mutable account collection.
func (p *AccountPool) uniqueAccountIndexesLocked() []int {
	if len(p.uniqueIndexes) > 0 || len(p.accounts) == 0 {
		return p.uniqueIndexes
	}
	seen := make(map[string]struct{}, len(p.accounts))
	indexes := make([]int, 0, len(p.accounts))
	for i := range p.accounts {
		if _, duplicate := seen[p.accounts[i].ID]; duplicate {
			continue
		}
		seen[p.accounts[i].ID] = struct{}{}
		indexes = append(indexes, i)
	}
	return indexes
}

// accountRanksBeforeLocked implements the former stable-sort comparison. The
// caller only replaces its current best on a strict improvement, preserving the
// first candidate when all configured tie-breakers are equal.
func (p *AccountPool) accountRanksBeforeLocked(candidate, current *config.Account, mode string) bool {
	switch mode {
	case config.SchedulingModeLeastRequests:
		candidateInFlight := p.inFlight[candidate.ID]
		currentInFlight := p.inFlight[current.ID]
		if candidateInFlight != currentInFlight {
			return candidateInFlight < currentInFlight
		}
		if candidate.LastUsed != current.LastUsed {
			return candidate.LastUsed < current.LastUsed
		}
		if candidate.RequestCount != current.RequestCount {
			return candidate.RequestCount < current.RequestCount
		}
		return effectiveWeight(candidate.Weight) > effectiveWeight(current.Weight)
	case config.SchedulingModeLeastCredits:
		candidateCredits := accountCreditUsage(*candidate)
		currentCredits := accountCreditUsage(*current)
		if candidateCredits != currentCredits {
			return candidateCredits < currentCredits
		}
		candidateInFlight := p.inFlight[candidate.ID]
		currentInFlight := p.inFlight[current.ID]
		if candidateInFlight != currentInFlight {
			return candidateInFlight < currentInFlight
		}
		return candidate.LastUsed < current.LastUsed
	default:
		candidateScore := accountCreditBalanceScore(*candidate)
		currentScore := accountCreditBalanceScore(*current)
		if candidateScore != currentScore {
			return candidateScore < currentScore
		}
		candidateInFlight := p.inFlight[candidate.ID]
		currentInFlight := p.inFlight[current.ID]
		if candidateInFlight != currentInFlight {
			return candidateInFlight < currentInFlight
		}
		if candidate.LastUsed != current.LastUsed {
			return candidate.LastUsed < current.LastUsed
		}
		return candidate.RequestCount < current.RequestCount
	}
}

func accountCreditUsage(acc config.Account) float64 {
	if acc.UsageCurrent > 0 {
		return acc.UsageCurrent
	}
	return acc.TotalCredits
}

func accountCreditBalanceScore(acc config.Account) float64 {
	weight := effectiveWeight(acc.Weight)
	return accountCreditUsage(acc) / float64(weight)
}

func (p *AccountPool) isSelectableLocked(acc *config.Account, model string, excluded map[string]bool, now time.Time, selection accountSelectionPolicy) bool {
	if acc == nil {
		return false
	}
	if excluded != nil && excluded[acc.ID] {
		return false
	}
	if model != "" && !p.accountHasModel(acc.ID, model) {
		return false
	}
	if !selection.accountAllowed(acc) {
		return false
	}
	if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
		return false
	}
	if tokenNeedsRefresh(acc, now) {
		return false
	}
	if isQuotaBlocked(*acc, selection.allowOverUsage) {
		return false
	}
	return true
}

func (p *AccountPool) isConcurrencyFullLocked(acc *config.Account) bool {
	if acc == nil || acc.MaxConcurrency <= 0 {
		return false
	}
	return p.inFlight[acc.ID] >= acc.MaxConcurrency
}

// SetModelList 缓存账号支持的模型集合（由 handler 在刷新后调用）
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		set[strings.ToLower(strings.TrimSpace(id))] = true
	}
	p.mu.Lock()
	p.modelLists[accountID] = set
	p.mu.Unlock()
}

// GetModelList 返回该账号缓存的模型 ID 列表（供 admin API 使用）。
// 若尚无缓存则返回空切片。
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// GetAllCachedModelIDs returns a sorted union of every cached account model ID.
func (p *AccountPool) GetAllCachedModelIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set := make(map[string]bool)
	for _, models := range p.modelLists {
		for id := range models {
			trimmed := strings.TrimSpace(id)
			if trimmed != "" {
				set[trimmed] = true
			}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// accountHasModel 检查账号是否支持指定模型。
// 若该账号尚无模型列表（冷启动），视为支持所有模型。
func (p *AccountPool) accountHasModel(accountID, model string) bool {
	list, ok := p.modelLists[accountID]
	if !ok || len(list) == 0 {
		return true // 冷启动：列表未就绪，乐观放行
	}
	return list[strings.ToLower(strings.TrimSpace(model))]
}

// GetNextForModel 获取下一个支持指定模型的可用账号（不占用并发）。
// model 应为去掉 thinking 后缀的实际模型名。
func (p *AccountPool) GetNextForModel(model string) *config.Account {
	return p.GetNextForModelExcluding(model, nil)
}

// GetNextForModelExcluding 获取下一个支持指定模型的可用账号（不占用并发），并跳过指定账号。
func (p *AccountPool) GetNextForModelExcluding(model string, excluded map[string]bool) *config.Account {
	mode := config.GetSchedulingConfig().Mode
	selection := loadAccountSelectionPolicy(model)
	p.mu.RLock()
	defer p.mu.RUnlock()
	acc, _ := p.selectNextLocked(model, excluded, false, mode, selection)
	return copyAccount(acc)
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return copyAccount(p.accountByIDLocked(id))
}

func (p *AccountPool) accountByIDLocked(id string) *config.Account {
	if indexes := p.accountIndexes[id]; len(indexes) > 0 {
		return &p.accounts[indexes[0]]
	}
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return &p.accounts[i]
		}
	}
	return nil
}

// RecordSuccess 记录请求成功，清除冷却
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
}

// RecordError 记录请求错误，设置冷却
func (p *AccountPool) RecordError(id string, isQuotaError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.errorCounts[id]++

	if isQuotaError {
		// 配额错误，冷却 1 小时
		p.cooldowns[id] = time.Now().Add(time.Hour)
	} else if p.errorCounts[id] >= 3 {
		// 连续 3 次错误，冷却 1 分钟
		p.cooldowns[id] = time.Now().Add(time.Minute)
	}
}

// CooldownAccount temporarily removes an account from scheduling for duration.
func (p *AccountPool) CooldownAccount(id string, duration time.Duration) {
	if id == "" || duration <= 0 {
		return
	}
	p.mu.Lock()
	p.cooldowns[id] = time.Now().Add(duration)
	p.errorCounts[id]++
	p.mu.Unlock()
}

// RecordRateLimit records a 429 without applying the legacy one-hour quota cooldown.
func (p *AccountPool) RecordRateLimit(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errorCounts[id]++
}

// IsAuthFailure reports whether an error indicates the refresh token / credentials
// have been revoked or invalidated upstream (401, 403 with auth markers, etc.).
// These accounts cannot be recovered automatically and must be re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	// Match HTTP status codes only when they appear as standalone tokens to avoid
	// false positives from arbitrary digits in the error body (e.g. request IDs).
	if hasStatusToken(msg, "401") || hasStatusToken(msg, "403") {
		return true
	}
	if strings.Contains(lower, "bad credentials") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid grant") ||
		strings.Contains(lower, "invalid_token") ||
		strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "token expired") ||
		strings.Contains(lower, "token has expired") ||
		strings.Contains(lower, "unauthorized") {
		return true
	}
	return false
}

// hasStatusToken returns true when status appears in s with non-digit boundaries
// on both sides, so "401" matches "HTTP 401 from ..." but not "request_401abc".
func hasStatusToken(s, status string) bool {
	for {
		idx := strings.Index(s, status)
		if idx < 0 {
			return false
		}
		leftOK := idx == 0 || !isDigit(s[idx-1])
		rightIdx := idx + len(status)
		rightOK := rightIdx >= len(s) || !isDigit(s[rightIdx])
		if leftOK && rightOK {
			return true
		}
		s = s[idx+len(status):]
	}
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// IsSuspensionError reports whether the error indicates the account has been
// temporarily suspended by upstream or has no available Kiro profile.
// Unlike auth failures (revoked credentials), these may be transient, but
// the account should be disabled until an operator re-enables it.
func IsSuspensionError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "temporarily_suspended") ||
		strings.Contains(lower, "temporarily suspended") ||
		strings.Contains(lower, "no available kiro profile")
}

// DisableAccount marks an account as disabled (auth revoked / unrecoverable),
// removes it from the in-memory pool so subsequent requests skip it, and
// persists the change via config.SetAccountBanStatus.
func (p *AccountPool) DisableAccount(id, reason string) {
	if err := config.SetAccountBanStatus(id, "DISABLED", reason); err != nil {
		// best effort — even if persistence fails, drop it from memory
		_ = err
	}
	p.mu.Lock()
	// Long cooldown as a safety net in case Reload races
	p.cooldowns[id] = time.Now().Add(24 * time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// MarkOverLimit marks an account as over usage limit (after a 402 / OVERAGE response).
// With the upstream OverageStatus model, the live status is refreshed via
// FetchOverageStatus from the request handler; here we just cooldown briefly so
// the next attempt picks a different account, then reload.
func (p *AccountPool) MarkOverLimit(id string) {
	p.mu.Lock()
	p.cooldowns[id] = time.Now().Add(time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.totalAccounts > 0 {
		return p.totalAccounts
	}

	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		seen[acc.ID] = true
	}
	return len(seen)
}

// AvailableCount 返回可用账号数
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	count := 0
	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if p.runtimeStateLocked(&acc, now).Available {
			count++
		}
	}
	return count
}

func (p *AccountPool) runtimeStateLocked(account *config.Account, now time.Time) RuntimeAccountState {
	if cooldown, ok := p.cooldowns[account.ID]; ok && now.Before(cooldown) {
		return RuntimeAccountState{Reason: "cooldown"}
	}
	if p.isConcurrencyFullLocked(account) {
		return RuntimeAccountState{Reason: "concurrency_full"}
	}
	return RuntimeAccountState{Available: true}
}

// RuntimeStates returns one state per unique account ID. Accounts omitted
// from the in-memory pool are not returned; callers can distinguish those
// records from disabled or quota-blocked configuration entries.
func (p *AccountPool) RuntimeStates() map[string]RuntimeAccountState {
	p.mu.RLock()
	defer p.mu.RUnlock()

	now := time.Now()
	states := make(map[string]RuntimeAccountState)
	for _, account := range p.accounts {
		if _, seen := states[account.ID]; seen {
			continue
		}
		states[account.ID] = p.runtimeStateLocked(&account, now)
	}
	return states
}

// RefillEligibleCount returns durable capacity for external refill controllers.
// Transient cooldowns and concurrency leases do not mean another account must
// be purchased; disabled and quota-blocked accounts are excluded by Reload.
func (p *AccountPool) RefillEligibleCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		seen[acc.ID] = true
	}
	return len(seen)
}

// UpdateStats 更新账号统计
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if indexes := p.accountIndexes[id]; len(indexes) > 0 {
		primary := &p.accounts[indexes[0]]
		primary.RequestCount++
		primary.TotalTokens += tokens
		primary.TotalCredits += credits
		primary.LastUsed = time.Now().Unix()
		for _, index := range indexes[1:] {
			p.accounts[index].RequestCount = primary.RequestCount
			p.accounts[index].ErrorCount = primary.ErrorCount
			p.accounts[index].TotalTokens = primary.TotalTokens
			p.accounts[index].TotalCredits = primary.TotalCredits
			p.accounts[index].LastUsed = primary.LastUsed
		}
		return
	}

	// Compatibility fallback for directly constructed pools that have not gone
	// through Reload and therefore do not have the production index metadata.
	var updated bool
	var requestCount, errorCount, totalTokens int
	var totalCredits float64
	var lastUsed int64
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			if !updated {
				p.accounts[i].RequestCount++
				p.accounts[i].TotalTokens += tokens
				p.accounts[i].TotalCredits += credits
				p.accounts[i].LastUsed = time.Now().Unix()

				requestCount = p.accounts[i].RequestCount
				errorCount = p.accounts[i].ErrorCount
				totalTokens = p.accounts[i].TotalTokens
				totalCredits = p.accounts[i].TotalCredits
				lastUsed = p.accounts[i].LastUsed
				updated = true
				continue
			}
			p.accounts[i].RequestCount = requestCount
			p.accounts[i].ErrorCount = errorCount
			p.accounts[i].TotalTokens = totalTokens
			p.accounts[i].TotalCredits = totalCredits
			p.accounts[i].LastUsed = lastUsed
		}
	}
	_ = updated
}

// SnapshotAccountStats returns one stats snapshot per unique account. The
// handler periodically flushes these snapshots to config.json in a single
// batched write instead of writing the config once per successful request.
func (p *AccountPool) SnapshotAccountStats() map[string]config.AccountStatsSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	stats := make(map[string]config.AccountStatsSnapshot)
	for i := range p.accounts {
		acc := p.accounts[i]
		if acc.ID == "" {
			continue
		}
		if _, seen := stats[acc.ID]; seen {
			continue
		}
		stats[acc.ID] = config.AccountStatsSnapshot{
			RequestCount: acc.RequestCount,
			ErrorCount:   acc.ErrorCount,
			TotalTokens:  acc.TotalTokens,
			TotalCredits: acc.TotalCredits,
			LastUsed:     acc.LastUsed,
		}
	}
	return stats
}

// WaitForPendingStats performs an explicit batched flush. Statistics normally
// stay in memory until Handler's 30-second/final flush, so this method no longer
// waits for or creates a per-request writer goroutine.
func (p *AccountPool) WaitForPendingStats() {
	if p == nil {
		return
	}
	_ = persistAccountStatsSnapshots(p.SnapshotAccountStats())
}

func persistAccountStatsSnapshots(stats map[string]config.AccountStatsSnapshot) error {
	var err error
	for attempt := 0; attempt < statsPersistAttempts; attempt++ {
		err = config.UpdateAccountStatsSnapshots(stats)
		if err == nil {
			return nil
		}
		if attempt+1 < statsPersistAttempts {
			time.Sleep(time.Duration(25*(1<<attempt)) * time.Millisecond)
		}
	}
	return err
}

// persistAccountStats is kept for package compatibility and tests. Runtime
// request paths use the batch function above.
func persistAccountStats(update accountStatsUpdate) error {
	return persistAccountStatsSnapshots(map[string]config.AccountStatsSnapshot{
		update.id: {
			RequestCount: update.requestCount,
			ErrorCount:   update.errorCount,
			TotalTokens:  update.totalTokens,
			TotalCredits: update.totalCredits,
			LastUsed:     update.lastUsed,
		},
	})
}

// GetAllAccounts 获取所有账号副本
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

func isOverUsageLimit(acc config.Account) bool {
	return acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit
}

// isQuotaBlocked reports whether an over-quota account should be skipped:
// the per-account upstream Overages switch (OverageStatus=ENABLED) and the
// global allowOverUsage setting are the two ways to keep it routable.
func isQuotaBlocked(acc config.Account, allowOverUsage bool) bool {
	return isOverUsageLimit(acc) && !isUpstreamOverageEnabled(acc) && !allowOverUsage
}

// isUpstreamOverageEnabled reports whether the upstream Overages switch is ON for this account.
// "ENABLED" → true; anything else (DISABLED, UNKNOWN, empty) → false.
func isUpstreamOverageEnabled(acc config.Account) bool {
	return strings.EqualFold(acc.OverageStatus, "ENABLED")
}

func effectiveWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	return weight
}

func copyAccount(acc *config.Account) *config.Account {
	if acc == nil {
		return nil
	}
	copy := *acc
	return &copy
}
