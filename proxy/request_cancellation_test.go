package proxy

import (
	"context"
	"errors"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDownstreamCancellationStopsUpstreamAndReleasesAccount(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const accountID = "cancel-release-test"
	if err := config.AddAccount(config.Account{
		ID:             accountID,
		Enabled:        true,
		AccessToken:    "token",
		ProfileArn:     "arn:aws:codewhisperer:profile/cancel-release-test",
		MaxConcurrency: 1,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	upstreamStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	defer func() {
		close(releaseUpstream)
		server.Close()
	}()
	defer swapKiroEndpointsForTest(t, server)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "wait",
		ModelID: "claude-sonnet-4.6",
		Origin:  "AI_EDITOR",
	}
	ctx, cancel := context.WithCancel(context.Background())
	bindKiroPayloadContext(payload, ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handleClaudeStream(
			httptest.NewRecorder(), payload, "claude-sonnet-4.6", "claude-sonnet-4-6",
			false, claudeThinkingResponseOptions{}, 1, nil, "",
		)
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	if got := p.InFlight(accountID); got != 1 {
		t.Fatalf("in-flight before cancel=%d, want 1", got)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after downstream cancellation")
	}
	if got := p.InFlight(accountID); got != 0 {
		t.Fatalf("in-flight after cancel=%d, want 0", got)
	}

	leased, _ := p.AcquireNextForModelExcluding("claude-sonnet-4.6", nil)
	if leased == nil || leased.ID != accountID {
		t.Fatalf("canceled request made healthy account unavailable: %#v", leased)
	}
	p.Release(leased.ID)
}

func TestRateLimitBackoffStopsOnDownstreamCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	payload := bindKiroPayloadContext(&KiroPayload{}, ctx)
	done := make(chan bool, 1)
	go func() {
		done <- (&Handler{}).waitBeforeRateLimitRetry(payload, rateLimitDecision{Delay: time.Hour})
	}()
	cancel()

	select {
	case completed := <-done:
		if completed {
			t.Fatal("canceled retry backoff reported completion")
		}
	case <-time.After(time.Second):
		t.Fatal("retry backoff ignored downstream cancellation")
	}
}

func TestPreResponseRetryBackoffStopsOnDownstreamCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	payload := bindKiroPayloadContext(&KiroPayload{}, ctx)
	cancel()
	retryCount := 0
	retrySameID := ""

	retried, err := (&Handler{}).retryKiroPreResponse(
		payload, &config.Account{ID: "cancel-retry"}, &retryCount, map[string]bool{}, &retrySameID,
	)
	if retried || !errors.Is(err, context.Canceled) {
		t.Fatalf("retried=%v err=%v, want canceled without retry", retried, err)
	}
	if retryCount != 0 || retrySameID != "" {
		t.Fatalf("canceled retry mutated state: count=%d same_id=%q", retryCount, retrySameID)
	}
}

func TestDownstreamCancellationStopsProfileLookupAndReleasesAccount(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const accountID = "cancel-profile-lookup"
	if err := config.AddAccount(config.Account{
		ID:             accountID,
		Enabled:        true,
		AccessToken:    "token",
		Region:         "us-east-1",
		MaxConcurrency: 1,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	lookupStarted := make(chan struct{})
	oldRestClient := kiroRestHttpStore.Load()
	kiroRestHttpStore.Store(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		select {
		case <-lookupStarted:
		default:
			close(lookupStarted)
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})})
	t.Cleanup(func() { kiroRestHttpStore.Store(oldRestClient) })

	modelCalls := 0
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		modelCalls++
	}))
	t.Cleanup(modelServer.Close)
	t.Cleanup(swapKiroEndpointsForTest(t, modelServer))

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	ctx, cancel := context.WithCancel(context.Background())
	payload := bindKiroPayloadContext(&KiroPayload{}, ctx)
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "wait",
		ModelID: "claude-sonnet-4.6",
		Origin:  "AI_EDITOR",
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handleClaudeStream(
			httptest.NewRecorder(), payload, "claude-sonnet-4.6", "claude-sonnet-4-6",
			false, claudeThinkingResponseOptions{}, 1, nil, "",
		)
	}()

	select {
	case <-lookupStarted:
	case <-time.After(time.Second):
		t.Fatal("profile lookup did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop after profile lookup cancellation")
	}
	if got := p.InFlight(accountID); got != 0 {
		t.Fatalf("in-flight after profile cancellation=%d, want 0", got)
	}
	if modelCalls != 0 {
		t.Fatalf("model endpoint calls=%d, want 0", modelCalls)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("context error=%v", ctx.Err())
	}
}

func TestClientRequestErrorDoesNotRotateAccounts(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, id := range []string{"client-error-a", "client-error-b"} {
		if err := config.AddAccount(config.Account{
			ID:          id,
			Enabled:     true,
			AccessToken: "token-" + id,
			ProfileArn:  "arn:aws:codewhisperer:profile/" + id,
		}); err != nil {
			t.Fatalf("add account %s: %v", id, err)
		}
	}

	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingStats)
	calls := 0
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		kiroCaller: func(*config.Account, *KiroPayload, *KiroStreamCallback) error {
			calls++
			return &KiroHTTPError{StatusCode: http.StatusBadRequest, Endpoint: "test", Body: "invalid request"}
		},
	}
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "invalid request",
		ModelID: "claude-sonnet-4.6",
		Origin:  "AI_EDITOR",
	}
	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(
		rec, payload, "claude-sonnet-4.6", "claude-sonnet-4-6",
		false, claudeThinkingResponseOptions{}, 1, nil, "",
	)

	if calls != 1 {
		t.Fatalf("upstream calls=%d, want 1 without account rotation", calls)
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
