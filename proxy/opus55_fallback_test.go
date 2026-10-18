package proxy

import (
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/pool"
)

func TestOpus55EmptyFallbackRewritesUpstreamOnly(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateOpus55EmptyMapToOpus5(true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"map-a", "map-b"} {
		if err := config.AddAccount(config.Account{ID: id, Enabled: true, AccessToken: "token"}); err != nil {
			t.Fatal(err)
		}
	}
	accountPool := pool.GetPool()
	accountPool.Reload()
	t.Cleanup(func() {
		_ = config.DeleteAccount("map-a")
		_ = config.DeleteAccount("map-b")
		pool.GetPool().Reload()
	})

	payload := &KiroPayload{}
	payload.ConversationState.ConversationID = "old-conversation"
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-opus-5.5"
	payload.ConversationState.History = []KiroHistoryMessage{{
		UserInputMessage: &KiroUserInputMessage{ModelID: "claude-opus-5-5", Content: "earlier"},
	}}
	payload.eventsNoneAccounts = map[string]struct{}{"map-a": {}, "map-b": {}}
	excluded := map[string]bool{"map-a": true, "map-b": true}
	handler := &Handler{pool: accountPool}

	if !handler.promoteOpus55EmptyToOpus5(payload, excluded) {
		t.Fatal("expected opus 5.5 empty fallback")
	}
	if payload.ConversationState.CurrentMessage.UserInputMessage.ModelID != "claude-opus-5" {
		t.Fatalf("upstream model = %s", payload.ConversationState.CurrentMessage.UserInputMessage.ModelID)
	}
	if payload.ConversationState.History[0].UserInputMessage.ModelID != "claude-opus-5" {
		t.Fatalf("history model = %s", payload.ConversationState.History[0].UserInputMessage.ModelID)
	}
	if payload.ConversationState.ConversationID == "old-conversation" || payload.ConversationState.ConversationID == "" {
		t.Fatalf("conversation id was not refreshed: %s", payload.ConversationState.ConversationID)
	}
	if len(excluded) != 0 {
		t.Fatalf("excluded accounts were kept: %v", excluded)
	}
	if handler.promoteOpus55EmptyToOpus5(payload, excluded) {
		t.Fatal("fallback ran a second time")
	}
	if got := accountModelForAttempt("claude-opus-5.5", payload); got != "claude-opus-5" {
		t.Fatalf("attempt model = %s", got)
	}
}

func TestOpus55CoolingPromotesAndAcquiresOpus5(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateOpus55EmptyMapToOpus5(true); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateEmptyOutputCooldown(true, 30); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cool-map-a", "cool-map-b"} {
		if err := config.AddAccount(config.Account{ID: id, Enabled: true, AccessToken: "token"}); err != nil {
			t.Fatal(err)
		}
	}
	accountPool := pool.GetPool()
	accountPool.Reload()
	t.Cleanup(func() {
		_ = config.DeleteAccount("cool-map-a")
		_ = config.DeleteAccount("cool-map-b")
		pool.GetPool().Reload()
	})
	accountPool.CooldownAccountModel("cool-map-a", "claude-opus-5.5", time.Minute)
	accountPool.CooldownAccountModel("cool-map-b", "claude-opus-5.5", time.Minute)

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-opus-5.5"
	excluded := map[string]bool{"cool-map-a": true}
	handler := &Handler{pool: accountPool}
	if !handler.promoteOpus55EmptyToOpus5(payload, excluded) {
		t.Fatal("expected cooling to promote opus 5.5")
	}
	if len(excluded) != 0 {
		t.Fatalf("excluded accounts were kept: %v", excluded)
	}
	attemptModel := accountModelForAttempt("claude-opus-5.5", payload)
	if attemptModel != "claude-opus-5" {
		t.Fatalf("attempt model = %s", attemptModel)
	}
	next, _ := accountPool.AcquireNextForModelExcluding(attemptModel, nil)
	if next == nil {
		t.Fatal("claude-opus-5 acquire was blocked by the 5.5 cooldown")
	}
	accountPool.Release(next.ID)
	if blocked, _ := accountPool.AcquireNextForModelExcluding("claude-opus-5.5", nil); blocked != nil {
		t.Fatalf("5.5 acquire = %s, want none", blocked.ID)
	}
}

func TestOpus55FallbackStaysOffWhileCooling(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateOpus55EmptyMapToOpus5(false); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateEmptyOutputCooldown(true, 30); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "cool-off", Enabled: true, AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	accountPool := pool.GetPool()
	accountPool.Reload()
	t.Cleanup(func() {
		_ = config.DeleteAccount("cool-off")
		pool.GetPool().Reload()
	})
	accountPool.CooldownAccountModel("cool-off", "claude-opus-5.5", time.Minute)
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-opus-5.5"
	handler := &Handler{pool: accountPool}
	if handler.promoteOpus55EmptyToOpus5(payload, map[string]bool{}) {
		t.Fatal("fallback promoted while the switch was off")
	}
	if got := handler.unavailableAccountError("claude-opus-5.5", false); got == nil || !isAllAccountsModelCoolingError(got) {
		t.Fatalf("unavailable = %v", got)
	}
}
