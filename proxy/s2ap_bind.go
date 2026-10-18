package proxy

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

type s2apCandidate struct {
	ID     string
	URL    string
	ExitIP string
}

var (
	s2apAssignMu sync.Mutex
	s2apCacheMu  sync.Mutex
	s2apCached   []s2apCandidate
	s2apCachedAt time.Time
)

// assignS2APProxy keeps a live hold, or draws a new one.
// An exit with no account is used before any exit is shared. Once every exit
// already has an account, the next account joins an exit that currently has
// the fewest accounts. The only account on an exit keeps that hold. An extra
// account on a busier exit moves to a lighter one, so a full pool never leaves
// a later account without a proxy.
func assignS2APProxy(now time.Time, hours int, lease config.S2APAccountLease, hasLease bool, available []s2apCandidate, exitLoad map[string]int, pick func(int) int) (config.S2APAccountLease, string, bool) {
	byID := make(map[string]s2apCandidate, len(available))
	for _, item := range available {
		if item.ID == "" || strings.TrimSpace(item.URL) == "" {
			continue
		}
		byID[item.ID] = item
	}
	if hasLease && now.Unix() < lease.Until {
		if item, ok := byID[lease.ProxyID]; ok && !s2apHoldShouldMove(item, byID, exitLoad) {
			if lease.ExitIP == "" && item.ExitIP != "" {
				lease.ExitIP = item.ExitIP
				return lease, item.URL, true
			}
			return lease, item.URL, false
		}
	}
	eligible := s2apLeastLoadedIDs(byID, exitLoad)
	if len(eligible) == 0 {
		return config.S2APAccountLease{}, "", false
	}
	index := 0
	if pick != nil && len(eligible) > 1 {
		index = pick(len(eligible))
	}
	if index < 0 || index >= len(eligible) {
		index = 0
	}
	if hours <= 0 {
		hours = 12
	}
	chosen := byID[eligible[index]]
	next := config.S2APAccountLease{
		ProxyID: chosen.ID,
		Until:   now.Add(time.Duration(hours) * time.Hour).Unix(),
		ExitIP:  chosen.ExitIP,
	}
	return next, chosen.URL, true
}

// s2apExitKey groups proxies that leave through the same address. A proxy
// whose exit address is still unknown stays in its own bucket.
func s2apExitKey(item s2apCandidate) string {
	if ip := strings.TrimSpace(item.ExitIP); ip != "" {
		return ip
	}
	return "proxy:" + item.ID
}

func s2apExitCount(exitLoad map[string]int, key string) int {
	if exitLoad == nil {
		return 0
	}
	return exitLoad[key]
}

// s2apHoldShouldMove reports that this account is an extra on an exit that
// has more accounts than some other exit. The sole account on an exit stays.
func s2apHoldShouldMove(item s2apCandidate, byID map[string]s2apCandidate, exitLoad map[string]int) bool {
	others := s2apExitCount(exitLoad, s2apExitKey(item))
	if others == 0 {
		return false
	}
	lightest, ok := s2apMinExitLoad(byID, exitLoad, s2apExitKey(item))
	return ok && others > lightest
}

func s2apMinExitLoad(byID map[string]s2apCandidate, exitLoad map[string]int, skip string) (int, bool) {
	seen := map[string]struct{}{}
	min := 0
	found := false
	for _, item := range byID {
		key := s2apExitKey(item)
		if key == skip {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		load := s2apExitCount(exitLoad, key)
		if !found || load < min {
			min = load
			found = true
		}
	}
	return min, found
}

func s2apLeastLoadedIDs(byID map[string]s2apCandidate, exitLoad map[string]int) []string {
	lightest, ok := s2apMinExitLoad(byID, exitLoad, "")
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(byID))
	for id, item := range byID {
		if s2apExitCount(exitLoad, s2apExitKey(item)) == lightest {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func s2apProxyForAccount(accountID string) string {
	if strings.TrimSpace(accountID) == "" || !config.S2APBindEnabled() {
		return ""
	}
	available := currentS2APCandidates()
	if len(available) == 0 {
		return ""
	}
	s2apAssignMu.Lock()
	defer s2apAssignMu.Unlock()
	now := time.Now()
	exitLoad := s2apBindingLoad(accountID, now, available)
	lease, hasLease := config.GetS2APAccountLease(accountID)
	next, proxyURL, changed := assignS2APProxy(now, config.S2APBindHours(), lease, hasLease, available, exitLoad, randomS2APIndex)
	if proxyURL == "" {
		return ""
	}
	if changed {
		if err := config.PutS2APAccountLease(accountID, next); err != nil {
			logger.Warnf("[S2AP] save account lease: %v", err)
		} else {
			logger.Infof("[S2AP] account %s bound exit %s until %d", accountID, next.ExitIP, next.Until)
		}
	}
	return proxyURL
}

// s2apBindingLoad counts other accounts on each exit. The account being
// assigned is omitted so its own hold is not treated as a reason to move.
func s2apBindingLoad(accountID string, now time.Time, available []s2apCandidate) map[string]int {
	byProxy := make(map[string]s2apCandidate, len(available))
	for _, item := range available {
		byProxy[item.ID] = item
	}
	load := map[string]int{}
	for id, lease := range config.SnapshotS2APAccountLeases() {
		if id == accountID || lease.ProxyID == "" || lease.Until <= now.Unix() {
			continue
		}
		exitIP := strings.TrimSpace(lease.ExitIP)
		if exitIP == "" {
			exitIP = byProxy[lease.ProxyID].ExitIP
		}
		key := exitIP
		if key == "" {
			key = "proxy:" + lease.ProxyID
		}
		load[key]++
	}
	return load
}

// rebalanceS2APLeases gives every enabled account an exit and moves extras
// toward exits that have fewer accounts. An exit is shared only after every
// exit already has an account.
func rebalanceS2APLeases() {
	if !config.S2APBindEnabled() {
		return
	}
	available := currentS2APCandidates()
	if len(available) == 0 {
		return
	}
	s2apAssignMu.Lock()
	defer s2apAssignMu.Unlock()
	now := time.Now()
	hours := config.S2APBindHours()
	idSet := map[string]struct{}{}
	for _, account := range config.GetAccounts() {
		if strings.TrimSpace(account.ID) == "" || !account.Enabled {
			continue
		}
		idSet[account.ID] = struct{}{}
	}
	for id, lease := range config.SnapshotS2APAccountLeases() {
		if lease.ProxyID == "" || lease.Until <= now.Unix() {
			continue
		}
		idSet[id] = struct{}{}
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		lease, hasLease := config.GetS2APAccountLease(id)
		exitLoad := s2apBindingLoad(id, now, available)
		next, proxyURL, changed := assignS2APProxy(now, hours, lease, hasLease, available, exitLoad, randomS2APIndex)
		if proxyURL == "" || !changed {
			continue
		}
		if err := config.PutS2APAccountLease(id, next); err != nil {
			logger.Warnf("[S2AP] rebalance %s: %v", id, err)
			continue
		}
		logger.Infof("[S2AP] account %s bound exit %s", id, next.ExitIP)
	}
}

func randomS2APIndex(n int) int {
	if n <= 1 {
		return 0
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return int(time.Now().UnixNano() % int64(n))
	}
	return int(binary.BigEndian.Uint64(buf[:]) % uint64(n))
}

func currentS2APCandidates() []s2apCandidate {
	s2apCacheMu.Lock()
	defer s2apCacheMu.Unlock()
	if time.Since(s2apCachedAt) < 30*time.Second && s2apCached != nil {
		return append([]s2apCandidate(nil), s2apCached...)
	}
	loaded, err := loadS2APCandidates()
	if err != nil {
		logger.Warnf("[S2AP] load proxies: %v", err)
		return append([]s2apCandidate(nil), s2apCached...)
	}
	s2apCached = loaded
	s2apCachedAt = time.Now()
	return append([]s2apCandidate(nil), loaded...)
}

func loadS2APCandidates() ([]s2apCandidate, error) {
	path := os.Getenv("KIRO_S2AP_PROXIES_FILE")
	if path == "" {
		path = "/opt/s2ap/data/proxies.json"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Proxies []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			URL     string `json:"url"`
			Enabled bool   `json:"enabled"`
		} `json:"proxies"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	online := s2apOnlineIDs()
	out := make([]s2apCandidate, 0, len(file.Proxies))
	for _, item := range file.Proxies {
		if !item.Enabled || strings.TrimSpace(item.URL) == "" || item.ID == "" {
			continue
		}
		exitIP := exitIPFromProxyName(item.Name)
		if len(online) > 0 {
			state, known := online[item.ID]
			if !known || !state.online {
				continue
			}
			if state.exitIP != "" {
				exitIP = state.exitIP
			}
		}
		out = append(out, s2apCandidate{ID: item.ID, URL: item.URL, ExitIP: exitIP})
	}
	return out, nil
}

func exitIPFromProxyName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(name); err == nil && net.ParseIP(host) != nil {
		return host
	}
	if net.ParseIP(name) != nil {
		return name
	}
	return ""
}

type s2apOnline struct {
	online bool
	exitIP string
}

type s2apAPIProxy struct {
	ID      string `json:"id"`
	Health  string `json:"health"`
	Enabled bool   `json:"enabled"`
	ExitIP  string `json:"exit_ip"`
}

func s2apOnlineIDs() map[string]s2apOnline {
	tokenPath := os.Getenv("KIRO_S2AP_ADMIN_TOKEN_FILE")
	if tokenPath == "" {
		tokenPath = "/opt/s2ap/data/admin.token"
	}
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil
	}
	api := os.Getenv("KIRO_S2AP_API")
	if api == "" {
		api = "http://127.0.0.1:33336"
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(api, "/")+"/api/proxies", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-Admin-Token", strings.TrimSpace(string(tokenBytes)))
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	var parsed struct {
		Proxies []s2apAPIProxy `json:"proxies"`
		Items   []s2apAPIProxy `json:"items"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	items := parsed.Proxies
	if len(items) == 0 {
		items = parsed.Items
	}
	out := make(map[string]s2apOnline, len(items))
	for _, item := range items {
		out[item.ID] = s2apOnline{
			online: item.Enabled && item.Health == "online",
			exitIP: item.ExitIP,
		}
	}
	return out
}

func s2apAvailableCount() int {
	return len(currentS2APCandidates())
}

func s2apExitByProxyID(id string) string {
	if id == "" {
		return ""
	}
	for _, item := range currentS2APCandidates() {
		if item.ID == id {
			return item.ExitIP
		}
	}
	return ""
}

// accountS2APLeaseView is what the account list shows. An expired hold is hidden.
func accountS2APLeaseView(lease config.S2APAccountLease, ok bool, now int64) (string, int64) {
	if !ok || lease.Until <= now {
		return "", 0
	}
	return strings.TrimSpace(lease.ExitIP), lease.Until
}
