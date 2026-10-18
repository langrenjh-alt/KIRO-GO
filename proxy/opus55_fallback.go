package proxy

import (
	"strings"

	"github.com/google/uuid"

	"kiro-go/config"
	"kiro-go/logger"
)

func noteEventsNoneAccount(payload *KiroPayload, accountID string) {
	if payload == nil || accountID == "" {
		return
	}
	if payload.eventsNoneAccounts == nil {
		payload.eventsNoneAccounts = map[string]struct{}{}
	}
	payload.eventsNoneAccounts[accountID] = struct{}{}
}

// accountModelForAttempt is the model used to lease an account. After the
// opus 5.5 fallback rewrites the payload, later attempts must lease with
// claude-opus-5. A 5.5 model cooldown would otherwise reject the retry
// before the rewritten body is sent. The downstream response model stays
// on the caller's original id.
func accountModelForAttempt(requested string, payload *KiroPayload) string {
	if payload != nil && payload.opus55EmptyFallbackUsed {
		if id := strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.ModelID); id != "" {
			return id
		}
	}
	return requested
}

// promoteOpus55EmptyToOpus5 rewrites the upstream model once opus 5.5 cannot
// be served: every enabled account returned events=none on this request, or
// every eligible account is inside the 5.5 model cooldown. The downstream
// model id is chosen by the caller and is left unchanged.
func (h *Handler) promoteOpus55EmptyToOpus5(payload *KiroPayload, excluded map[string]bool) bool {
	if h == nil || h.pool == nil || payload == nil || payload.opus55EmptyFallbackUsed {
		return false
	}
	if !config.Opus55EmptyMapToOpus5Enabled() || !isOpus55Model(payload) {
		return false
	}
	model := payload.ConversationState.CurrentMessage.UserInputMessage.ModelID
	noted := h.pool.AllEnabledModelAccountsNoted(model, payload.eventsNoneAccounts)
	cooling := h.pool.AllEligibleAccountsModelCooling(model)
	if !noted && !cooling {
		return false
	}
	rewriteOpus55UpstreamToOpus5(payload)
	for id := range excluded {
		delete(excluded, id)
	}
	if noted {
		logger.Warnf("[KiroAPI] all accounts returned events=none for %s; retrying upstream as claude-opus-5", model)
	} else {
		logger.Warnf("[KiroAPI] all accounts are cooling for %s; retrying upstream as claude-opus-5", model)
	}
	return true
}

func rewriteOpus55UpstreamToOpus5(payload *KiroPayload) {
	replaceModel := func(modelID string) string {
		switch strings.ToLower(strings.TrimSpace(modelID)) {
		case "claude-opus-5.5", "claude-opus-5-5":
			return "claude-opus-5"
		default:
			return modelID
		}
	}
	current := &payload.ConversationState.CurrentMessage.UserInputMessage
	current.ModelID = replaceModel(current.ModelID)
	for i := range payload.ConversationState.History {
		if message := payload.ConversationState.History[i].UserInputMessage; message != nil {
			message.ModelID = replaceModel(message.ModelID)
		}
	}
	payload.ConversationState.ConversationID = uuid.NewString()
	payload.ConversationState.AgentContinuationId = uuid.NewString()
	payload.opus55EmptyFallbackUsed = true
	payload.eventsNoneAccounts = nil
}
