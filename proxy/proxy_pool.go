package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/egress"
	"kiro-go/logger"
)

type proxyInspectionState struct {
	Running   bool `json:"running"`
	Total     int  `json:"total"`
	Completed int  `json:"completed"`
}

type proxyPoolController struct {
	mu     sync.Mutex
	state  proxyInspectionState
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	check  func(context.Context, config.ProxyEntry) config.ProxyCheckResult
}

func (h *Handler) startProxyPoolWorker() {
	ctx, cancel := context.WithCancel(context.Background())
	h.proxyPool = &proxyPoolController{ctx: ctx, cancel: cancel, check: inspectProxy}
	h.backgroundWG.Add(1)
	go func() {
		defer h.backgroundWG.Done()
		defer func() {
			// Synchronize with an inspection that passed its context check just
			// before cancellation. Its Add must finish before Wait sees zero.
			h.proxyPool.mu.Lock()
			h.proxyPool.mu.Unlock()
			h.proxyPool.wg.Wait()
		}()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		next := time.Now().Add(time.Second)
		for {
			select {
			case <-ctx.Done():
				if err := egress.FlushTraffic(); err != nil {
					logger.Warnf("[ProxyPool] flush traffic: %v", err)
				}
				return
			case now := <-tick.C:
				if now.Unix()%5 == 0 {
					if err := egress.FlushTraffic(); err != nil {
						logger.Warnf("[ProxyPool] flush traffic: %v", err)
					}
				}
				if !now.Before(next) {
					h.startProxyInspection(nil, false)
					next = now.Add(time.Duration(config.GetProxyPoolSettings().CheckIntervalSeconds) * time.Second)
				}
			}
		}
	}()
}

// The scheduler only enqueues work. Bounded workers perform network probes off
// the request path; the next tick cannot overlap the previous inspection.
func (h *Handler) startProxyInspection(ids []string, includeDisabled bool) bool {
	c := h.proxyPool
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx == nil {
		c.ctx, c.cancel = context.WithCancel(context.Background())
	}
	if c.ctx.Err() != nil || c.state.Running {
		return false
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	entries := make([]config.ProxyEntry, 0)
	for _, entry := range config.GetProxies() {
		if len(selected) > 0 && !selected[entry.ID] {
			continue
		}
		if !includeDisabled && !entry.Enabled {
			continue
		}
		entries = append(entries, entry)
	}
	c.state = proxyInspectionState{Running: len(entries) > 0, Total: len(entries)}
	if len(entries) == 0 {
		return true
	}
	checker := c.check
	if checker == nil {
		checker = inspectProxy
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() { c.mu.Lock(); c.state.Running = false; c.mu.Unlock() }()
		workers := config.GetProxyPoolSettings().CheckConcurrency
		if workers < 1 {
			workers = 16
		}
		if workers > len(entries) {
			workers = len(entries)
		}
		jobs := make(chan config.ProxyEntry)
		var wg sync.WaitGroup
		for n := 0; n < workers; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for entry := range jobs {
					result := checker(c.ctx, entry)
					// Shutdown/caller cancellation is not evidence of a dead proxy.
					if c.ctx.Err() == nil {
						if err := config.RecordProxyCheck(entry.ID, entry.URL, result); err != nil {
							logger.Warnf("[ProxyPool] save inspection: %v", err)
						}
					}
					c.mu.Lock()
					c.state.Completed++
					c.mu.Unlock()
				}
			}()
		}
		for _, entry := range entries {
			select {
			case jobs <- entry:
			case <-c.ctx.Done():
				close(jobs)
				wg.Wait()
				return
			}
		}
		close(jobs)
		wg.Wait()
	}()
	return true
}

var proxyGeoCache sync.Map

type proxyGeoCacheEntry struct {
	ProxyURL  string
	ExitIP    string
	Country   string
	Region    string
	City      string
	CheckedAt time.Time
}

func inspectProxy(ctx context.Context, entry config.ProxyEntry) config.ProxyCheckResult {
	return inspectProxyEndpoints(ctx, entry, "https://www.cloudflare.com/cdn-cgi/trace", []string{"https://ipwho.is/", "https://ipapi.co/json/"})
}

func inspectProxyEndpoints(parent context.Context, entry config.ProxyEntry, traceURL string, geoURLs []string) config.ProxyCheckResult {
	started := time.Now()
	result := config.ProxyCheckResult{CheckedAt: started.Unix()}
	normalized, err := config.NormalizeProxyURL(entry.URL)
	if err != nil {
		result.LastError = "Invalid proxy URL"
		return result
	}
	entry.URL = normalized
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 24*time.Second)
	defer cancel()
	transport := buildKiroTransport(entry.URL)
	transport.ResponseHeaderTimeout = 8 * time.Second
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 9 * time.Second}
	get := func(target string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, errors.New("probe network request failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("probe HTTP %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return nil, errors.New("probe response read failed")
		}
		return body, nil
	}
	trace, traceErr := get(traceURL)
	if traceErr == nil {
		for _, line := range strings.Split(string(trace), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch key {
			case "ip":
				if net.ParseIP(strings.TrimSpace(value)) != nil {
					result.ExitIP = strings.TrimSpace(value)
				}
			case "loc":
				result.Country = strings.TrimSpace(value)
			}
		}
		result.Healthy = result.ExitIP != ""
	}
	var previous proxyGeoCacheEntry
	if cached, ok := proxyGeoCache.Load(entry.ID); ok {
		previous, _ = cached.(proxyGeoCacheEntry)
	}
	cacheMatches := result.Healthy && previous.ProxyURL == entry.URL && previous.ExitIP == result.ExitIP && previous.City != ""
	if cacheMatches && time.Since(previous.CheckedAt) < 6*time.Hour {
		result.Country, result.Region, result.City = previous.Country, previous.Region, previous.City
		result.LatencyMS = time.Since(started).Milliseconds()
		return result
	}
	// Geo services can throttle without the proxy being broken. A successful
	// independent trace keeps it healthy even when geographic metadata is pending.
	for _, endpoint := range geoURLs {
		body, err := get(endpoint)
		if err != nil {
			continue
		}
		var geo struct {
			IP          string `json:"ip"`
			Success     *bool  `json:"success"`
			Error       bool   `json:"error"`
			Country     string `json:"country"`
			CountryName string `json:"country_name"`
			Region      string `json:"region"`
			City        string `json:"city"`
		}
		if json.Unmarshal(body, &geo) != nil || geo.Error || (geo.Success != nil && !*geo.Success) || net.ParseIP(geo.IP) == nil {
			continue
		}
		if result.ExitIP != "" && result.ExitIP != geo.IP {
			continue
		}
		result.Healthy = true
		result.ExitIP = geo.IP
		result.Country = geo.Country
		if geo.CountryName != "" {
			result.Country = geo.CountryName
		}
		result.Region = geo.Region
		result.City = geo.City
		// Keep at most one location per proxy, even for rotating exit IPs.
		proxyGeoCache.Store(entry.ID, proxyGeoCacheEntry{ProxyURL: entry.URL, ExitIP: result.ExitIP, Country: result.Country, Region: result.Region, City: result.City, CheckedAt: time.Now()})
		break
	}
	if result.Healthy && result.City == "" {
		// Metadata throttling must not erase a known city for the same exit.
		if cacheMatches {
			result.Country, result.Region, result.City = previous.Country, previous.Region, previous.City
			result.LastError = "GeoIP refresh pending; retained previous location"
		} else if result.ExitIP == entry.ExitIP && entry.City != "" {
			result.Country, result.Region, result.City = entry.Country, entry.Region, entry.City
			result.LastError = "GeoIP refresh pending; retained previous location"
		}
	}
	result.LatencyMS = time.Since(started).Milliseconds()
	if !result.Healthy {
		result.LastError = "Proxy connectivity check failed"
	} else if result.City == "" {
		result.LastError = "GeoIP lookup pending; connectivity verified"
	}
	return result
}

type proxyImportError struct {
	Line  int    `json:"line"`
	Error string `json:"error"`
}

func normalizePoolProxyURL(raw, scheme string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = scheme + "://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return "", errors.New("invalid proxy URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "socks5" {
		return "", errors.New("scheme must be http or socks5")
	}
	p, err := strconv.Atoi(u.Port())
	if u.Hostname() == "" || err != nil || p < 1 || p > 65535 || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("expected host:port without path/query")
	}
	u.Path = ""
	u.Host = net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(p))
	return u.String(), nil
}

func parseProxyImport(text, scheme string) ([]config.ProxyEntry, []proxyImportError) {
	if scheme == "" {
		scheme = "http"
	}
	entries := make([]config.ProxyEntry, 0)
	problems := make([]proxyImportError, 0)
	for index, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, raw := "", line
		if left, right, ok := strings.Cut(line, "|"); ok {
			name, raw = strings.TrimSpace(left), strings.TrimSpace(right)
		}
		normalized, err := normalizePoolProxyURL(raw, scheme)
		if err != nil {
			problems = append(problems, proxyImportError{index + 1, err.Error()})
			continue
		}
		entries = append(entries, config.ProxyEntry{Name: name, URL: normalized, Enabled: true, Health: "unknown"})
	}
	return entries, problems
}

func (h *Handler) proxyPoolView() map[string]interface{} {
	bindings := config.GetProxyBindings()
	counts := map[string]int{}
	for _, id := range bindings {
		counts[id]++
	}
	items := make([]map[string]interface{}, 0)
	for _, entry := range egress.ProxyEntriesWithTraffic() {
		body, _ := json.Marshal(entry)
		item := map[string]interface{}{}
		_ = json.Unmarshal(body, &item)
		stats := egress.Stats(entry.ID)
		item["accountCount"] = counts[entry.ID]
		item["currentConcurrency"] = stats.CurrentConcurrency
		item["currentBytesPerSecond"] = stats.CurrentBytesPerSecond
		item["totalBytes"] = entry.TotalBytes
		items = append(items, item)
	}
	state := proxyInspectionState{}
	if h.proxyPool != nil {
		h.proxyPool.mu.Lock()
		state = h.proxyPool.state
		h.proxyPool.mu.Unlock()
	}
	return map[string]interface{}{"settings": config.GetProxyPoolSettings(), "proxies": items, "inspection": state}
}

func proxyAPIError(w http.ResponseWriter, status int, err error) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func decodeProxyRequest(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		proxyAPIError(w, 400, errors.New("invalid request JSON"))
		return false
	}
	var extra interface{}
	if d.Decode(&extra) != io.EOF {
		proxyAPIError(w, 400, errors.New("expected one JSON object"))
		return false
	}
	return true
}

func (h *Handler) apiGetProxyPool(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(h.proxyPoolView())
}

func (h *Handler) apiProxyPoolSettings(w http.ResponseWriter, r *http.Request) {
	var patch config.ProxyPoolSettingsPatch
	if !decodeProxyRequest(w, r, &patch) {
		return
	}
	if (patch.CheckIntervalSeconds != nil && (*patch.CheckIntervalSeconds < 10 || *patch.CheckIntervalSeconds > 3600)) || (patch.CheckConcurrency != nil && (*patch.CheckConcurrency < 1 || *patch.CheckConcurrency > 64)) {
		proxyAPIError(w, 400, errors.New("interval must be 10..3600 seconds; concurrency must be 1..64"))
		return
	}
	if _, err := config.PatchProxyPoolSettings(patch); err != nil {
		proxyAPIError(w, 500, err)
		return
	}
	h.pool.Reload()
	_ = json.NewEncoder(w).Encode(h.proxyPoolView())
}

func (h *Handler) apiImportProxies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text          string `json:"text"`
		DefaultScheme string `json:"defaultScheme"`
	}
	if !decodeProxyRequest(w, r, &req) {
		return
	}
	if req.DefaultScheme != "" && req.DefaultScheme != "http" && req.DefaultScheme != "socks5" {
		proxyAPIError(w, 400, errors.New("invalid default scheme"))
		return
	}
	entries, problems := parseProxyImport(req.Text, req.DefaultScheme)
	if len(entries) > 10000 {
		proxyAPIError(w, 400, errors.New("maximum 10000 entries per import"))
		return
	}
	added, skipped, err := config.ImportProxies(entries)
	if err != nil {
		proxyAPIError(w, 500, err)
		return
	}
	h.startProxyInspection(nil, false)
	view := h.proxyPoolView()
	view["imported"] = added
	view["duplicates"] = skipped
	view["errors"] = problems
	_ = json.NewEncoder(w).Encode(view)
}

func (h *Handler) apiCheckProxies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decodeProxyRequest(w, r, &req) {
		return
	}
	if !h.startManualProxyInspection(w, req.IDs) {
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(h.proxyPoolView())
}

func (h *Handler) apiBatchProxies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string   `json:"action"`
		IDs    []string `json:"ids"`
		Patch  struct {
			Name     *string `json:"name"`
			URL      *string `json:"url"`
			Username *string `json:"username"`
			Password *string `json:"password"`
			Scheme   *string `json:"scheme"`
		} `json:"patch"`
	}
	if !decodeProxyRequest(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		proxyAPIError(w, 400, errors.New("select at least one proxy"))
		return
	}
	var err error
	switch req.Action {
	case "check":
		if !h.startManualProxyInspection(w, req.IDs) {
			return
		}
		w.WriteHeader(202)
	case "delete":
		err = config.DeleteProxies(req.IDs)
	case "enable", "disable":
		enabled := req.Action == "enable"
		err = config.UpdateProxies(req.IDs, config.ProxyPatch{Enabled: &enabled})
	case "edit":
		// Construct and validate every URL before changing any selected entry.
		patches := make(map[string]config.ProxyPatch)
		selected := map[string]bool{}
		for _, id := range req.IDs {
			selected[id] = true
		}
		for _, entry := range config.GetProxies() {
			if !selected[entry.ID] {
				continue
			}
			p := config.ProxyPatch{Name: req.Patch.Name}
			raw := entry.URL
			if req.Patch.URL != nil {
				raw = *req.Patch.URL
			}
			normalized, e := normalizePoolProxyURL(raw, "http")
			if e != nil {
				proxyAPIError(w, 400, e)
				return
			}
			u, _ := url.Parse(normalized)
			if req.Patch.Scheme != nil {
				u.Scheme = *req.Patch.Scheme
			}
			username, password := "", ""
			if u.User != nil {
				username = u.User.Username()
				password, _ = u.User.Password()
			}
			if req.Patch.Username != nil {
				username = *req.Patch.Username
			}
			if req.Patch.Password != nil {
				password = *req.Patch.Password
			}
			if username != "" || password != "" {
				u.User = url.UserPassword(username, password)
			} else {
				u.User = nil
			}
			normalized, e = normalizePoolProxyURL(u.String(), "http")
			if e != nil {
				proxyAPIError(w, 400, e)
				return
			}
			p.URL = &normalized
			patches[entry.ID] = p
		}
		for _, id := range req.IDs {
			if _, ok := patches[id]; !ok {
				proxyAPIError(w, 400, errors.New("proxy not found"))
				return
			}
		}
		err = config.UpdateProxyPatches(patches)
	default:
		proxyAPIError(w, 400, errors.New("unknown proxy action"))
		return
	}
	if err != nil {
		proxyAPIError(w, 400, err)
		return
	}
	h.pool.Reload()
	view := h.proxyPoolView()
	view["success"] = true
	_ = json.NewEncoder(w).Encode(view)
}

func (h *Handler) startManualProxyInspection(w http.ResponseWriter, ids []string) bool {
	available := make(map[string]bool)
	for _, entry := range config.GetProxies() {
		available[entry.ID] = true
	}
	for _, id := range ids {
		if !available[id] {
			proxyAPIError(w, http.StatusBadRequest, errors.New("proxy not found"))
			return false
		}
	}
	if !h.startProxyInspection(ids, true) {
		proxyAPIError(w, http.StatusConflict, errors.New("proxy inspection is already running or stopping"))
		return false
	}
	return true
}
