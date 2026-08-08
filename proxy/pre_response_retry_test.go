package proxy

import (
	"errors"
	"testing"
	"time"

	"kiro-go/config"
)

func TestPreResponseTransportRetryIsBoundedToOne(t *testing.T) {
	payload := &KiroPayload{}
	account := &config.Account{ID: "transport-retry"}
	retryCount := 0
	retrySameID := ""

	retried, err := (&Handler{}).retryKiroPreResponse(
		payload, account, &retryCount, map[string]bool{}, &retrySameID, errors.New("stream INTERNAL_ERROR"),
	)
	if err != nil || !retried || retryCount != 1 || retrySameID != account.ID {
		t.Fatalf("first retry: retried=%v err=%v count=%d same_id=%q", retried, err, retryCount, retrySameID)
	}

	retrySameID = ""
	retried, err = (&Handler{}).retryKiroPreResponse(
		payload, account, &retryCount, map[string]bool{}, &retrySameID, errors.New("stream INTERNAL_ERROR"),
	)
	if err != nil || retried || retryCount != 1 || retrySameID != "" {
		t.Fatalf("second retry: retried=%v err=%v count=%d same_id=%q", retried, err, retryCount, retrySameID)
	}
}

func TestPreResponseMarkerRetriesDoNotConsumeTransportBudget(t *testing.T) {
	payload := completionProtocolTestPayload()
	account := &config.Account{ID: "marker-retry"}
	transportRetries := maxKiroTransportPreResponseRetries
	retrySameID := ""
	mismatch := newUpstreamAgentMarkerMismatchError()

	for semanticRetry := 1; semanticRetry <= maxKiroAgentCompletionRetries; semanticRetry++ {
		prepareReasoningOnlyRetry(payload, mismatch)
		retried, err := (&Handler{}).retryKiroPreResponse(
			payload, account, &transportRetries, map[string]bool{}, &retrySameID, mismatch,
		)
		if err != nil || !retried || payload.AgentCompletionRetries != semanticRetry ||
			transportRetries != maxKiroTransportPreResponseRetries || retrySameID != account.ID {
			t.Fatalf("semantic retry %d: retried=%v err=%v semantic=%d transport=%d same_id=%q",
				semanticRetry, retried, err, payload.AgentCompletionRetries, transportRetries, retrySameID)
		}
	}

	prepareReasoningOnlyRetry(payload, mismatch)
	retried, err := (&Handler{}).retryKiroPreResponse(
		payload, account, &transportRetries, map[string]bool{}, &retrySameID, mismatch,
	)
	if err != nil || !retried || payload.AgentCompletionRetries != maxKiroAgentCompletionRetries+1 ||
		payload.AgentFullContextRecoveries != 1 || !payload.agentFullContextRecoveryPending ||
		transportRetries != maxKiroTransportPreResponseRetries {
		t.Fatalf("full-context semantic retry: retried=%v err=%v semantic=%d full=%d pending=%v transport=%d",
			retried, err, payload.AgentCompletionRetries, payload.AgentFullContextRecoveries,
			payload.agentFullContextRecoveryPending, transportRetries)
	}

	retrySameID = ""
	prepareReasoningOnlyRetry(payload, mismatch)
	retried, err = (&Handler{}).retryKiroPreResponse(
		payload, account, &transportRetries, map[string]bool{}, &retrySameID, mismatch,
	)
	if err != nil || retried || payload.AgentCompletionRetries != maxKiroAgentCompletionRetries+2 ||
		payload.agentFullContextRecoveryPending || transportRetries != maxKiroTransportPreResponseRetries {
		t.Fatalf("exhausted full-context retry: retried=%v err=%v semantic=%d pending=%v transport=%d",
			retried, err, payload.AgentCompletionRetries, payload.agentFullContextRecoveryPending, transportRetries)
	}
}

func TestPreResponseToolJSONDoesNotUseTransportRetry(t *testing.T) {
	payload := &KiroPayload{}
	account := &config.Account{ID: "tool-json-retry"}
	retryCount := 0
	retrySameID := ""
	toolJSONErr := &KiroStreamProtocolError{
		Code: "upstream_tool_json_error",
		Err:  errors.New("truncated tool JSON"),
	}

	retried, err := (&Handler{}).retryKiroPreResponse(
		payload, account, &retryCount, map[string]bool{}, &retrySameID, toolJSONErr,
	)
	if err != nil || retried || retryCount != 0 || retrySameID != "" {
		t.Fatalf("retried=%v err=%v count=%d same_id=%q", retried, err, retryCount, retrySameID)
	}
}

func TestPreResponseRetryProgressUsesIndependentBudgets(t *testing.T) {
	payload := completionProtocolTestPayload()
	payload.AgentCompletionRetries = 2
	if got, limit := kiroPreResponseRetryProgress(payload, 1, newUpstreamAgentMarkerMismatchError()); got != 2 || limit != maxKiroAgentCompletionRetries {
		t.Fatalf("marker progress=%d/%d", got, limit)
	}
	if got, limit := kiroPreResponseRetryProgress(payload, 1, errors.New("transport")); got != 1 || limit != maxKiroTransportPreResponseRetries {
		t.Fatalf("transport progress=%d/%d", got, limit)
	}
	payload.AgentFullContextRecoveries = 1
	payload.agentFullContextRecoveryPending = true
	if got, limit := kiroPreResponseRetryProgress(payload, 1, newUpstreamAgentActionOnlyError()); got != 1 || limit != maxKiroAgentFullContextRecoveries {
		t.Fatalf("full-context progress=%d/%d", got, limit)
	}
	if got, limit := kiroPreResponseRetryProgress(payload, 1, newUpstreamAgentMarkerMismatchError()); got != 1 || limit != maxKiroAgentFullContextRecoveries {
		t.Fatalf("full-context marker progress=%d/%d", got, limit)
	}
	if phase := kiroAgentRecoveryPhase(payload, newUpstreamAgentActionOnlyError()); phase != "full_context" {
		t.Fatalf("full-context phase=%q", phase)
	}
}

func TestPreResponseRecoveryTimeoutDoesNotRetry(t *testing.T) {
	payload := completionProtocolTestPayload()
	account := &config.Account{ID: "recovery-timeout"}
	retryCount := 0
	retrySameID := ""

	retried, err := (&Handler{}).retryKiroPreResponse(
		payload,
		account,
		&retryCount,
		map[string]bool{},
		&retrySameID,
		newUpstreamRecoveryTimeoutError(time.Second),
	)
	if err != nil || retried || retryCount != 0 || retrySameID != "" {
		t.Fatalf("retried=%v err=%v retryCount=%d retrySameID=%q", retried, err, retryCount, retrySameID)
	}
}
