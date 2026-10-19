package config

import (
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrProxyNotFound           = errors.New("proxy not found")
	ErrInvalidProxy            = errors.New("invalid proxy")
	ErrNoAvailableProxy        = errors.New("no available proxy")
	ErrAccountProxyUnavailable = errors.New("account is disabled, banned, or not configured")
)

const (
	ProxyHealthUnknown   = "unknown"
	ProxyHealthHealthy   = "healthy"
	ProxyHealthUnhealthy = "unhealthy"

	ProxyDisabledManual  = "manual"
	ProxyDisabledHealth  = "health"
	ProxyDisabledInvalid = "invalid"
)

// ProxyEntry contains durable proxy state. Live concurrency and bandwidth are
// transport counters, not configuration, and are joined by the admin API.
type ProxyEntry struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	URL          string `json:"url"`
	Enabled      bool   `json:"enabled"`
	DisabledBy   string `json:"disabledBy,omitempty"`
	StateVersion uint64 `json:"stateVersion,omitempty"`
	Health       string `json:"health"`
	ExitIP       string `json:"exitIP,omitempty"`
	Country      string `json:"country,omitempty"`
	Region       string `json:"region,omitempty"`
	City         string `json:"city,omitempty"`
	LastChecked  int64  `json:"lastChecked,omitempty"`
	LatencyMS    int64  `json:"latencyMS,omitempty"`
	LastError    string `json:"lastError,omitempty"`
	TotalBytes   int64  `json:"totalBytes"`
}

type ProxyPoolSettings struct {
	Enabled              bool `json:"enabled"`
	BlockWithoutProxy    bool `json:"blockWithoutProxy"`
	CheckIntervalSeconds int  `json:"checkIntervalSeconds"`
	CheckConcurrency     int  `json:"checkConcurrency"`
}

type ProxyPoolSettingsPatch struct {
	Enabled              *bool `json:"enabled,omitempty"`
	BlockWithoutProxy    *bool `json:"blockWithoutProxy,omitempty"`
	CheckIntervalSeconds *int  `json:"checkIntervalSeconds,omitempty"`
	CheckConcurrency     *int  `json:"checkConcurrency,omitempty"`
}

type ProxyPatch struct {
	Name    *string `json:"name,omitempty"`
	URL     *string `json:"url,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

type ProxyCheckResult struct {
	// ExpectedStateVersion fences probes started before an administrator edit or
	// an automatic disable/recovery. Metadata-only observations do not invalidate
	// in-flight streams; timestamps still order their check observations.
	ExpectedStateVersion *uint64 `json:"-"`
	Healthy              bool    `json:"healthy"`
	ExitIP               string  `json:"exitIP,omitempty"`
	Country              string  `json:"country,omitempty"`
	Region               string  `json:"region,omitempty"`
	City                 string  `json:"city,omitempty"`
	LatencyMS            int64   `json:"latencyMS"`
	LastError            string  `json:"lastError,omitempty"`
	CheckedAt            int64   `json:"checkedAt"`
}

func defaultProxyPoolSettings() ProxyPoolSettings {
	return ProxyPoolSettings{BlockWithoutProxy: true, CheckIntervalSeconds: 60, CheckConcurrency: 16}
}

func normalizeProxyPoolSettings(settings ProxyPoolSettings) ProxyPoolSettings {
	if settings.CheckIntervalSeconds <= 0 {
		settings.CheckIntervalSeconds = 60
	}
	if settings.CheckConcurrency <= 0 {
		settings.CheckConcurrency = 16
	}
	return settings
}

// normalizeLoadedProxyPoolLocked migrates only the old on/off switch. No
// external-service exit, lease, or proxy source is ever imported or consulted.
func normalizeLoadedProxyPoolLocked(data []byte) bool {
	var source struct {
		Pool          json.RawMessage `json:"proxyPoolSettings"`
		LegacyEnabled *bool           `json:"s2apBindEnabled"`
		LegacyLeases  json.RawMessage `json:"s2apAccountLeases"`
		LegacyHours   json.RawMessage `json:"s2apBindHours"`
	}
	_ = json.Unmarshal(data, &source)
	dirty := source.LegacyEnabled != nil || source.LegacyLeases != nil || source.LegacyHours != nil
	if source.Pool == nil || string(source.Pool) == "null" {
		cfg.ProxyPoolSettings = defaultProxyPoolSettings()
		if source.LegacyEnabled != nil {
			cfg.ProxyPoolSettings.Enabled = *source.LegacyEnabled
		}
		dirty = true
	} else {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(source.Pool, &fields)
		if _, present := fields["blockWithoutProxy"]; !present {
			cfg.ProxyPoolSettings.BlockWithoutProxy = true
			dirty = true
		}
		normalized := normalizeProxyPoolSettings(cfg.ProxyPoolSettings)
		if normalized != cfg.ProxyPoolSettings {
			cfg.ProxyPoolSettings = normalized
			dirty = true
		}
	}
	ids := make(map[string]struct{}, len(cfg.Proxies))
	for i := range cfg.Proxies {
		proxy := &cfg.Proxies[i]
		previous := *proxy
		if _, duplicate := ids[proxy.ID]; proxy.ID == "" || duplicate {
			proxy.ID = newUniqueConfigID(ids)
			dirty = true
		}
		ids[proxy.ID] = struct{}{}
		normalized, err := NormalizeProxyURL(proxy.URL)
		if err != nil {
			if proxy.Enabled || proxy.Health != ProxyHealthUnhealthy || proxy.DisabledBy != ProxyDisabledInvalid || proxy.LastError != "invalid proxy URL" {
				dirty = true
			}
			proxy.Enabled = false
			proxy.DisabledBy = ProxyDisabledInvalid
			proxy.Health = ProxyHealthUnhealthy
			proxy.LastError = "invalid proxy URL"
		} else if normalized != proxy.URL {
			proxy.URL = normalized
			dirty = true
		}
		if proxy.Health == "" {
			proxy.Health = ProxyHealthUnknown
			dirty = true
		}
		if proxy.Health == ProxyHealthUnhealthy && proxy.Enabled {
			proxy.Enabled = false
			proxy.DisabledBy = ProxyDisabledHealth
			dirty = true
		}
		// Older versions never recorded why a proxy was disabled. Preserve those
		// administrator decisions unless an explicit one-time migration marks a
		// known automatic fault as health-disabled. A saved healthy observation is
		// not a new connectivity check and must not enable a proxy during startup.
		if proxy.Enabled {
			if proxy.DisabledBy != "" {
				proxy.DisabledBy = ""
				dirty = true
			}
		} else if proxy.DisabledBy != ProxyDisabledManual && proxy.DisabledBy != ProxyDisabledHealth && proxy.DisabledBy != ProxyDisabledInvalid {
			proxy.DisabledBy = ProxyDisabledManual
			dirty = true
		}
		if proxy.StateVersion == 0 {
			proxy.StateVersion = 1
			dirty = true
		} else if previous.Enabled != proxy.Enabled || previous.DisabledBy != proxy.DisabledBy || previous.URL != proxy.URL || previous.Health != proxy.Health {
			proxy.StateVersion++
			dirty = true
		}
		if proxy.TotalBytes < 0 {
			proxy.TotalBytes = 0
			dirty = true
		}
	}
	return reconcileProxyBindingsLocked() || dirty
}

// NormalizeProxyURL accepts HTTP(S) and SOCKS5 URLs with optional credentials.
// SOCKS5H is normalized to SOCKS5; both resolve target hostnames at the proxy.
func NormalizeProxyURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%w: malformed URL", ErrInvalidProxy)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme == "socks5h" {
		u.Scheme = "socks5"
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" {
		return "", fmt.Errorf("%w: scheme must be http, https or socks5", ErrInvalidProxy)
	}
	if u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%w: expected scheme://[user:password@]host:port", ErrInvalidProxy)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("%w: port must be between 1 and 65535", ErrInvalidProxy)
	}
	u.Host = net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(port))
	u.Path, u.RawPath = "", ""
	return u.String(), nil
}

func GetProxyPoolSettings() ProxyPoolSettings {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultProxyPoolSettings()
	}
	return normalizeProxyPoolSettings(cfg.ProxyPoolSettings)
}

func UpdateProxyPoolSettings(settings ProxyPoolSettings) (ProxyPoolSettings, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return updateProxyPoolSettingsLocked(settings)
}

// PatchProxyPoolSettings merges under the same lock as persistence, preventing
// simultaneous edits of independent switches from overwriting each other.
func PatchProxyPoolSettings(patch ProxyPoolSettingsPatch) (ProxyPoolSettings, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ProxyPoolSettings{}, errors.New("config not initialized")
	}
	settings := cfg.ProxyPoolSettings
	if patch.Enabled != nil {
		settings.Enabled = *patch.Enabled
	}
	if patch.BlockWithoutProxy != nil {
		settings.BlockWithoutProxy = *patch.BlockWithoutProxy
	}
	if patch.CheckIntervalSeconds != nil {
		settings.CheckIntervalSeconds = *patch.CheckIntervalSeconds
	}
	if patch.CheckConcurrency != nil {
		settings.CheckConcurrency = *patch.CheckConcurrency
	}
	return updateProxyPoolSettingsLocked(settings)
}

func updateProxyPoolSettingsLocked(settings ProxyPoolSettings) (ProxyPoolSettings, error) {
	settings = normalizeProxyPoolSettings(settings)
	if settings.CheckIntervalSeconds < 10 || settings.CheckIntervalSeconds > 3600 {
		return ProxyPoolSettings{}, fmt.Errorf("checkIntervalSeconds must be between 10 and 3600")
	}
	if settings.CheckConcurrency > 128 {
		return ProxyPoolSettings{}, fmt.Errorf("checkConcurrency must be between 1 and 128")
	}
	if cfg == nil {
		return ProxyPoolSettings{}, errors.New("config not initialized")
	}
	previous := snapshotProxyPoolLocked()
	cfg.ProxyPoolSettings = settings
	reconcileProxyBindingsLocked()
	if err := saveLocked(); err != nil {
		restoreProxyPoolLocked(previous)
		return ProxyPoolSettings{}, err
	}
	return settings, nil
}

func GetProxies() []ProxyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return []ProxyEntry{}
	}
	return append([]ProxyEntry{}, cfg.Proxies...)
}

// ImportProxies validates the entire batch before mutating anything and skips
// duplicate URLs. IDs, health and traffic cannot be supplied by an import.
func ImportProxies(entries []ProxyEntry) (added, skipped int, err error) {
	entries = append([]ProxyEntry(nil), entries...)
	for i := range entries {
		entries[i].URL, err = NormalizeProxyURL(entries[i].URL)
		if err != nil {
			return 0, 0, fmt.Errorf("proxy %d: %w", i+1, err)
		}
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return 0, 0, errors.New("config not initialized")
	}
	previous := snapshotProxyPoolLocked()
	seen := make(map[string]bool, len(cfg.Proxies)+len(entries))
	for _, entry := range cfg.Proxies {
		seen[entry.URL] = true
	}
	for _, entry := range entries {
		if seen[entry.URL] {
			skipped++
			continue
		}
		seen[entry.URL] = true
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			u, _ := url.Parse(entry.URL)
			name = u.Host
		}
		imported := ProxyEntry{ID: newUUID(), Name: name, URL: entry.URL, Enabled: entry.Enabled, Health: ProxyHealthUnknown, StateVersion: 1}
		if !imported.Enabled {
			imported.DisabledBy = ProxyDisabledManual
		}
		cfg.Proxies = append(cfg.Proxies, imported)
		added++
	}
	if added == 0 {
		return 0, skipped, nil
	}
	reconcileProxyBindingsLocked()
	if err = saveLocked(); err != nil {
		restoreProxyPoolLocked(previous)
		return 0, skipped, err
	}
	return added, skipped, nil
}

func proxyIDsLocked(ids []string) (map[string]bool, error) {
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[strings.TrimSpace(id)] = true
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("%w: select at least one proxy", ErrInvalidProxy)
	}
	found := make(map[string]bool, len(selected))
	for _, entry := range cfg.Proxies {
		if selected[entry.ID] {
			found[entry.ID] = true
		}
	}
	if len(found) != len(selected) {
		return nil, ErrProxyNotFound
	}
	return selected, nil
}

func UpdateProxies(ids []string, patch ProxyPatch) error {
	patches := make(map[string]ProxyPatch, len(ids))
	for _, id := range ids {
		patches[strings.TrimSpace(id)] = patch
	}
	if patch.URL != nil && len(patches) != 1 {
		return fmt.Errorf("%w: URL may only be changed for one proxy", ErrInvalidProxy)
	}
	return UpdateProxyPatches(patches)
}

// UpdateProxyPatches commits a heterogeneous batch as one transaction. The
// candidate collection is validated in full, including URL uniqueness.
func UpdateProxyPatches(patches map[string]ProxyPatch) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	normalizedPatches := make(map[string]ProxyPatch, len(patches))
	ids := make([]string, 0, len(patches))
	for id, patch := range patches {
		id = strings.TrimSpace(id)
		if _, duplicate := normalizedPatches[id]; duplicate {
			return fmt.Errorf("%w: duplicate proxy ID", ErrInvalidProxy)
		}
		normalizedPatches[id] = patch
		ids = append(ids, id)
	}
	selected, err := proxyIDsLocked(ids)
	if err != nil {
		return err
	}
	candidate := append([]ProxyEntry(nil), cfg.Proxies...)
	for i := range candidate {
		entry := &candidate[i]
		if !selected[entry.ID] {
			continue
		}
		patch := normalizedPatches[entry.ID]
		if patch.Name != nil {
			entry.Name = strings.TrimSpace(*patch.Name)
		}
		if patch.URL != nil {
			normalizedURL, err := NormalizeProxyURL(*patch.URL)
			if err != nil {
				return err
			}
			if entry.URL != normalizedURL {
				entry.URL = normalizedURL
				entry.Health = ProxyHealthUnknown
				entry.ExitIP = ""
				entry.Country = ""
				entry.Region = ""
				entry.City = ""
				entry.LastChecked = time.Now().Unix()
				entry.LatencyMS = 0
				entry.LastError = ""
				if entry.DisabledBy == ProxyDisabledInvalid {
					entry.DisabledBy = ProxyDisabledManual
				}
			}
		}
		if patch.Enabled != nil {
			if *patch.Enabled {
				if _, err := NormalizeProxyURL(entry.URL); err != nil {
					return err
				}
			}
			if entry.Enabled != *patch.Enabled {
				entry.LastChecked = time.Now().Unix()
			}
			entry.Enabled = *patch.Enabled
			if entry.Enabled {
				entry.DisabledBy = ""
			} else {
				entry.DisabledBy = ProxyDisabledManual
			}
			if entry.Enabled && entry.Health == ProxyHealthUnhealthy {
				entry.Health = ProxyHealthUnknown
				entry.LastError = ""
			}
		}
		// An idempotent explicit action is still a newer administrator decision.
		// Advance once per transaction even if the URL/on-off value is unchanged.
		if patch.URL != nil || patch.Enabled != nil {
			entry.StateVersion++
		}
	}
	seen := make(map[string]bool, len(candidate))
	for _, entry := range candidate {
		if seen[entry.URL] {
			return fmt.Errorf("%w: URL already exists", ErrInvalidProxy)
		}
		seen[entry.URL] = true
	}
	previous := snapshotProxyPoolLocked()
	cfg.Proxies = candidate
	reconcileProxyBindingsLocked()
	if err := saveLocked(); err != nil {
		restoreProxyPoolLocked(previous)
		return err
	}
	return nil
}

func DeleteProxies(ids []string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	selected, err := proxyIDsLocked(ids)
	if err != nil {
		return err
	}
	previous := snapshotProxyPoolLocked()
	kept := make([]ProxyEntry, 0, len(cfg.Proxies)-len(selected))
	for _, entry := range cfg.Proxies {
		if !selected[entry.ID] {
			kept = append(kept, entry)
		}
	}
	cfg.Proxies = kept
	reconcileProxyBindingsLocked()
	if err := saveLocked(); err != nil {
		restoreProxyPoolLocked(previous)
		return err
	}
	return nil
}

func RecordProxyCheck(id, expectedURL string, result ProxyCheckResult) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.Proxies {
		entry := &cfg.Proxies[i]
		if entry.ID != id {
			continue
		}
		if entry.URL != expectedURL || (result.ExpectedStateVersion != nil && *result.ExpectedStateVersion != entry.StateVersion) || (result.CheckedAt > 0 && result.CheckedAt < entry.LastChecked) {
			return nil
		}
		previous := snapshotProxyPoolLocked()
		entry.LastChecked = result.CheckedAt
		if entry.LastChecked == 0 {
			entry.LastChecked = time.Now().Unix()
		}
		entry.LatencyMS = result.LatencyMS
		if result.Healthy {
			entry.Health = ProxyHealthHealthy
			entry.LastError = SanitizeProxyError(result.LastError, entry.URL)
			entry.ExitIP, entry.Country, entry.Region, entry.City = result.ExitIP, result.Country, result.Region, result.City
			if entry.DisabledBy == ProxyDisabledHealth {
				entry.Enabled = true
				entry.DisabledBy = ""
			}
		} else {
			entry.Health = ProxyHealthUnhealthy
			if entry.Enabled || entry.DisabledBy == ProxyDisabledHealth {
				entry.DisabledBy = ProxyDisabledHealth
			}
			entry.Enabled = false
			entry.LastError = SanitizeProxyError(result.LastError, entry.URL)
			if entry.LastError == "" {
				entry.LastError = "proxy connectivity check failed"
			}
		}
		if previousEntry := previous.proxies[i]; entry.Enabled != previousEntry.Enabled || entry.DisabledBy != previousEntry.DisabledBy {
			entry.StateVersion++
		}
		reconcileProxyBindingsLocked()
		if err := saveLocked(); err != nil {
			restoreProxyPoolLocked(previous)
			return err
		}
		return nil
	}
	return nil // A concurrently deleted entry must not be recreated by a probe.
}

func DisableProxy(id, expectedURL, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return disableProxyLocked(id, expectedURL, nil, reason)
}

// DisableProxyIfCurrent rejects a network failure from a request that used an
// older proxy state. In particular, a late response cannot undo a manual enable
// or a newer automatic recovery even when both happened in the same second.
func DisableProxyIfCurrent(id, expectedURL string, version uint64, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return disableProxyLocked(id, expectedURL, &version, reason)
}

func disableProxyLocked(id, expectedURL string, version *uint64, reason string) error {
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.Proxies {
		entry := &cfg.Proxies[i]
		if entry.ID != id {
			continue
		}
		if entry.URL != expectedURL || (version != nil && *version != entry.StateVersion) {
			return nil
		}
		previous := snapshotProxyPoolLocked()
		if entry.Enabled || entry.DisabledBy == ProxyDisabledHealth {
			entry.DisabledBy = ProxyDisabledHealth
		}
		entry.Enabled = false
		entry.Health = ProxyHealthUnhealthy
		entry.LastError = SanitizeProxyError(reason, entry.URL)
		entry.LastChecked = time.Now().Unix()
		if previousEntry := previous.proxies[i]; entry.Enabled != previousEntry.Enabled || entry.DisabledBy != previousEntry.DisabledBy {
			entry.StateVersion++
		}
		reconcileProxyBindingsLocked()
		if err := saveLocked(); err != nil {
			restoreProxyPoolLocked(previous)
			return err
		}
		return nil
	}
	return nil
}

func proxyUsable(entry ProxyEntry) bool { return entry.Enabled && entry.Health != ProxyHealthUnhealthy }

// SanitizeProxyError removes configured authentication material before an
// upstream network error is stored or displayed in administration responses.
func SanitizeProxyError(message, proxyURL string) string {
	u, err := url.Parse(proxyURL)
	if err != nil || u.User == nil {
		return message
	}
	redacted := *u
	redacted.User = nil
	message = strings.ReplaceAll(message, proxyURL, redacted.String())
	if password, ok := u.User.Password(); ok && password != "" {
		message = strings.ReplaceAll(message, url.QueryEscape(password), "***")
		message = strings.ReplaceAll(message, password, "***")
	}
	message = strings.ReplaceAll(message, u.User.String(), "***")
	return message
}

func proxyAccountActive(account Account) bool {
	status := strings.ToUpper(strings.TrimSpace(account.BanStatus))
	return account.Enabled && status != "BANNED" && status != "DISABLED" && status != "SUSPENDED"
}

type proxyAssignmentCandidate struct {
	id           string
	count, order int
}
type proxyAssignmentQueue []proxyAssignmentCandidate

func (q proxyAssignmentQueue) Len() int { return len(q) }
func (q proxyAssignmentQueue) Less(i, j int) bool {
	if q[i].count != q[j].count {
		return q[i].count < q[j].count
	}
	return q[i].order < q[j].order
}
func (q proxyAssignmentQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *proxyAssignmentQueue) Push(v any)   { *q = append(*q, v.(proxyAssignmentCandidate)) }
func (q *proxyAssignmentQueue) Pop() any {
	old := *q
	v := old[len(old)-1]
	*q = old[:len(old)-1]
	return v
}

// reconcileProxyBindingsLocked preserves valid assignments, then fills missing
// assignments in config order with the least-used proxy (unused first). Tier,
// subscription and quota are intentionally not considered.
func reconcileProxyBindingsLocked() bool {
	if cfg == nil {
		return false
	}
	usable := make(map[string]bool, len(cfg.Proxies))
	counts := make(map[string]int, len(cfg.Proxies))
	for _, entry := range cfg.Proxies {
		if proxyUsable(entry) {
			usable[entry.ID] = true
		}
	}
	active := make(map[string]bool, len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		if proxyAccountActive(account) && account.ID != "" {
			active[account.ID] = true
		}
	}
	changed := false
	for accountID, proxyID := range cfg.AccountProxyBindings {
		if !cfg.ProxyPoolSettings.Enabled || !active[accountID] || !usable[proxyID] {
			delete(cfg.AccountProxyBindings, accountID)
			changed = true
		} else {
			counts[proxyID]++
		}
	}
	if !cfg.ProxyPoolSettings.Enabled {
		return changed
	}
	candidates := make(proxyAssignmentQueue, 0, len(usable))
	for index, entry := range cfg.Proxies {
		if usable[entry.ID] {
			candidates = append(candidates, proxyAssignmentCandidate{id: entry.ID, count: counts[entry.ID], order: index})
		}
	}
	heap.Init(&candidates)
	for _, account := range cfg.Accounts {
		if !active[account.ID] || cfg.AccountProxyBindings[account.ID] != "" {
			continue
		}
		if len(candidates) == 0 {
			break
		}
		chosen := candidates[0].id
		if cfg.AccountProxyBindings == nil {
			cfg.AccountProxyBindings = make(map[string]string)
		}
		cfg.AccountProxyBindings[account.ID] = chosen
		candidates[0].count++
		heap.Fix(&candidates, 0)
		changed = true
	}
	return changed
}

func GetProxyBindings() map[string]string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	out := make(map[string]string)
	if cfg != nil {
		for account, proxy := range cfg.AccountProxyBindings {
			out[account] = proxy
		}
	}
	return out
}

func ReconcileProxyBindings() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	previous := snapshotProxyPoolLocked()
	if !reconcileProxyBindingsLocked() {
		return nil
	}
	if err := saveLocked(); err != nil {
		restoreProxyPoolLocked(previous)
		return err
	}
	return nil
}

func ResolveAccountProxy(accountID string) (ProxyEntry, bool, error) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return resolveAccountProxyLocked(accountID)
}

// ResolveAccountProxyPolicy snapshots the shared switch and assignment in one
// read, so a cached transport does not combine two different setting versions.
func ResolveAccountProxyPolicy(accountID string) (ProxyPoolSettings, ProxyEntry, bool, error) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	settings := defaultProxyPoolSettings()
	if cfg != nil {
		settings = normalizeProxyPoolSettings(cfg.ProxyPoolSettings)
	}
	entry, bound, err := resolveAccountProxyLocked(accountID)
	return settings, entry, bound, err
}

func resolveAccountProxyLocked(accountID string) (ProxyEntry, bool, error) {
	if cfg == nil || !cfg.ProxyPoolSettings.Enabled {
		return ProxyEntry{}, false, nil
	}
	active := false
	for _, account := range cfg.Accounts {
		if account.ID == accountID {
			active = proxyAccountActive(account)
			break
		}
	}
	if !active {
		return ProxyEntry{}, false, ErrAccountProxyUnavailable
	}
	proxyID := cfg.AccountProxyBindings[accountID]
	for _, entry := range cfg.Proxies {
		if entry.ID == proxyID && proxyUsable(entry) {
			return entry, true, nil
		}
	}
	if cfg.ProxyPoolSettings.BlockWithoutProxy {
		return ProxyEntry{}, false, ErrNoAvailableProxy
	}
	return ProxyEntry{}, false, nil
}

// ProxyPoolAccountPolicySnapshot is read before the account-pool lock so
// scheduling never nests configuration and account-pool mutexes.
func ProxyPoolAccountPolicySnapshot() (required bool, allowed map[string]bool) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || !cfg.ProxyPoolSettings.Enabled || !cfg.ProxyPoolSettings.BlockWithoutProxy {
		return false, nil
	}
	usable := make(map[string]bool, len(cfg.Proxies))
	for _, entry := range cfg.Proxies {
		usable[entry.ID] = proxyUsable(entry)
	}
	allowed = make(map[string]bool, len(cfg.AccountProxyBindings))
	for _, account := range cfg.Accounts {
		if proxyAccountActive(account) && usable[cfg.AccountProxyBindings[account.ID]] {
			allowed[account.ID] = true
		}
	}
	return true, allowed
}

func AccountProxyAllowed(accountID string) bool {
	required, allowed := ProxyPoolAccountPolicySnapshot()
	return !required || allowed[accountID]
}

func AddProxyTraffic(deltas map[string]int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	previous := snapshotProxyPoolLocked()
	changed := false
	for i := range cfg.Proxies {
		delta := deltas[cfg.Proxies[i].ID]
		if delta <= 0 {
			continue
		}
		if cfg.Proxies[i].TotalBytes > math.MaxInt64-delta {
			cfg.Proxies[i].TotalBytes = math.MaxInt64
		} else {
			cfg.Proxies[i].TotalBytes += delta
		}
		changed = true
	}
	if !changed {
		return nil
	}
	if err := saveLocked(); err != nil {
		restoreProxyPoolLocked(previous)
		return err
	}
	return nil
}

type proxyPoolSnapshot struct {
	settings ProxyPoolSettings
	proxies  []ProxyEntry
	bindings map[string]string
}

func snapshotProxyPoolLocked() proxyPoolSnapshot {
	out := proxyPoolSnapshot{settings: cfg.ProxyPoolSettings, proxies: append([]ProxyEntry(nil), cfg.Proxies...)}
	if cfg.AccountProxyBindings != nil {
		out.bindings = make(map[string]string, len(cfg.AccountProxyBindings))
		for account, proxy := range cfg.AccountProxyBindings {
			out.bindings[account] = proxy
		}
	}
	return out
}

func restoreProxyPoolLocked(previous proxyPoolSnapshot) {
	cfg.ProxyPoolSettings, cfg.Proxies, cfg.AccountProxyBindings = previous.settings, previous.proxies, previous.bindings
}

// saveAccountMutationLocked keeps account lifecycle and assignments atomic on
// disk as well as in memory. Callers capture the snapshots before mutation.
func saveAccountMutationLocked(accounts []Account, previous proxyPoolSnapshot) error {
	reconcileProxyBindingsLocked()
	if err := saveLocked(); err != nil {
		cfg.Accounts = accounts
		restoreProxyPoolLocked(previous)
		return err
	}
	return nil
}
