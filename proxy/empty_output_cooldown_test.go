package proxy

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/pool"
)

func TestEventsNoneEmptyOutputCoolsAccountAndSwitches(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateEmptyOutputCooldown(true, 30); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cool-a", "cool-b"} {
		if err := config.AddAccount(config.Account{ID: id, Enabled: true, AccessToken: "token"}); err != nil {
			t.Fatal(err)
		}
	}
	accountPool := pool.GetPool()
	accountPool.Reload()
	t.Cleanup(func() {
		_ = config.DeleteAccount("cool-a")
		_ = config.DeleteAccount("cool-b")
		pool.GetPool().Reload()
	})

	handler := &Handler{pool: accountPool}
	excluded := map[string]bool{}
	retrySameID := "cool-a"
	retryCount := 0
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-opus-5.5"
	cause := fmt.Errorf("%w (events=none assistant_bytes=0 reasoning_bytes=0 stop=-)", errUpstreamEmptyResponse)

	retried, err := handler.retryKiroPreResponse(payload, &config.Account{ID: "cool-a"}, &retryCount, excluded, &retrySameID, cause)
	if err != nil || !retried || retrySameID != "" || !excluded["cool-a"] || retryCount != 0 {
		t.Fatalf("switch: retried=%v err=%v same=%q excluded=%v count=%d", retried, err, retrySameID, excluded, retryCount)
	}
	next, _ := accountPool.AcquireNextForModelExcluding("claude-opus-5.5", nil)
	if next == nil || next.ID != "cool-b" {
		t.Fatalf("next account = %#v, want cool-b", next)
	}
	accountPool.Release(next.ID)

	accountPool.CooldownAccountModel("cool-b", "claude-opus-5.5", time.Minute)
	if !accountPool.AllEligibleAccountsModelCooling("claude-opus-5.5") {
		t.Fatal("expected both accounts to be cooling")
	}
	if got := handler.unavailableAccountError("claude-opus-5.5", false); got == nil || !isAllAccountsModelCoolingError(got) {
		t.Fatalf("unavailable = %v", got)
	}
}

func TestEventsNoneRetriesAnotherAccountWhenCooldownIsOff(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateEmptyOutputCooldown(false, 30); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"retry-a", "retry-b"} {
		if err := config.AddAccount(config.Account{ID: id, Enabled: true, AccessToken: "token"}); err != nil {
			t.Fatal(err)
		}
	}
	accountPool := pool.GetPool()
	accountPool.Reload()
	t.Cleanup(func() {
		_ = config.DeleteAccount("retry-a")
		_ = config.DeleteAccount("retry-b")
		pool.GetPool().Reload()
	})

	handler := &Handler{pool: accountPool}
	excluded := map[string]bool{}
	retrySameID := "retry-a"
	retryCount := 0
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-opus-5.5"
	cause := fmt.Errorf("%w (events=none assistant_bytes=0 reasoning_bytes=0 stop=-)", errUpstreamEmptyResponse)

	retried, err := handler.retryKiroPreResponse(payload, &config.Account{ID: "retry-a"}, &retryCount, excluded, &retrySameID, cause)
	if err != nil || !retried || retrySameID != "" || !excluded["retry-a"] || retryCount != 0 {
		t.Fatalf("retry: retried=%v err=%v same=%q excluded=%v count=%d", retried, err, retrySameID, excluded, retryCount)
	}
	for _, item := range accountPool.ListModelCooldowns(time.Now()) {
		if item.AccountID == "retry-a" || item.AccountID == "retry-b" {
			t.Fatalf("cooldown was recorded while disabled: %+v", item)
		}
	}
	next, _ := accountPool.AcquireNextForModelExcluding("claude-opus-5.5", excluded)
	if next == nil || next.ID != "retry-b" {
		t.Fatalf("next account = %#v, want retry-b", next)
	}
	accountPool.Release(next.ID)
}

func TestDisabledCooldownReleasesExistingModelCooldown(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateEmptyOutputCooldown(true, 30); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "release-a", Enabled: true, AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	accountPool := pool.GetPool()
	accountPool.Reload()
	t.Cleanup(func() {
		_ = config.DeleteAccount("release-a")
		pool.GetPool().Reload()
	})
	accountPool.CooldownAccountModel("release-a", "claude-opus-5.5", time.Minute)
	if got, _ := accountPool.AcquireNextForModelExcluding("claude-opus-5.5", nil); got != nil {
		accountPool.Release(got.ID)
		t.Fatalf("cooled account was selected: %s", got.ID)
	}
	if err := config.UpdateEmptyOutputCooldown(false, 30); err != nil {
		t.Fatal(err)
	}
	got, _ := accountPool.AcquireNextForModelExcluding("claude-opus-5.5", nil)
	if got == nil || got.ID != "release-a" {
		t.Fatalf("account after disabling cooldown = %#v", got)
	}
	accountPool.Release(got.ID)
}

func TestOtherEmptyOutputDoesNotCoolTheModel(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateEmptyOutputCooldown(true, 30); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{pool: pool.GetPool()}
	excluded := map[string]bool{}
	retrySameID := ""
	retryCount := 0
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-opus-5.5"
	retried, err := handler.retryKiroPreResponse(payload, &config.Account{ID: "plain-empty"}, &retryCount, excluded, &retrySameID, errUpstreamEmptyResponse)
	if err != nil || !retried || retrySameID != "plain-empty" || excluded["plain-empty"] {
		t.Fatalf("plain empty: retried=%v err=%v same=%q excluded=%v", retried, err, retrySameID, excluded)
	}
}
