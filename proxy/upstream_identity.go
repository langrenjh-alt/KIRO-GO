package proxy

import (
	"crypto/sha256"
	"fmt"

	"kiro-go/config"
)

// newUUIDv4FromMaterial builds a stable UUID whose version nibble is 4.
// A SHA-1 name-based UUID (version 5) is uncommon for an IDE session id and
// is identical for every account that sends the same prompt.
func newUUIDv4FromMaterial(material []byte) string {
	sum := sha256.Sum256(material)
	return formatUUIDv4(sum[:16])
}

func formatUUIDv4(raw []byte) string {
	var id [16]byte
	copy(id[:], raw)
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}

func scopedConversationID(accountID, base string) string {
	return newUUIDv4FromMaterial([]byte(accountID + "\n" + base))
}

// bindUpstreamConversation keeps one downstream conversation stable on the
// account that is actually calling Kiro, and gives a different account a
// different id. A handler that deliberately replaces the id, such as a fresh
// recovery conversation, is preserved.
func bindUpstreamConversation(payload *KiroPayload, account *config.Account) {
	if payload == nil || account == nil {
		return
	}
	accountID := account.ID
	if accountID == "" {
		return
	}
	current := payload.ConversationState.ConversationID
	if payload.conversationIDBase == "" {
		payload.conversationIDBase = current
	}
	if payload.conversationIDBase == "" {
		payload.conversationIDBase = newUUIDv4FromMaterial([]byte(accountID + "\nempty"))
	}
	if current != "" && current != payload.conversationIDScoped && current != payload.conversationIDBase {
		payload.conversationIDBase = current
	}
	expected := scopedConversationID(accountID, payload.conversationIDBase)
	if current == expected && payload.conversationIDScopeAccount == accountID {
		return
	}
	payload.ConversationState.ConversationID = expected
	payload.conversationIDScoped = expected
	payload.conversationIDScopeAccount = accountID
	refreshScopedAssistantMessageIDs(payload)
}

func refreshScopedAssistantMessageIDs(payload *KiroPayload) {
	if payload == nil || payload.HistoryMode != kiroHistoryModeNativeActive {
		return
	}
	for i := range payload.ConversationState.History {
		assistant := payload.ConversationState.History[i].AssistantResponseMessage
		if assistant == nil || assistant.MessageID == "" {
			continue
		}
		assistant.MessageID = stableKiroAssistantMessageID(payload.ConversationState.ConversationID, i, assistant)
	}
}
