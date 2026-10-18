package proxy

import (
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/pool"
)

func TestEmptyOutputModelViewsFollowTheSwitch(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateEmptyOutputCooldown(true, 30); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	items := []pool.ModelCooldown{
		{AccountID: "acct-a", Model: "claude-opus-5.5", Until: until},
		{AccountID: "acct-a", Model: "claude-opus-5", Until: until.Add(time.Minute)},
		{AccountID: "acct-b", Model: "claude-opus-4.8", Until: until},
		{AccountID: "", Model: "claude-opus-5.5", Until: until},
	}
	views := emptyOutputModelViews(items)
	if len(views["acct-a"]) != 2 || views["acct-a"][0].Model != "claude-opus-5.5" || views["acct-a"][0].Until != until.Unix() {
		t.Fatalf("acct-a views = %#v", views["acct-a"])
	}
	if len(views["acct-b"]) != 1 || views["acct-b"][0].Model != "claude-opus-4.8" {
		t.Fatalf("acct-b views = %#v", views["acct-b"])
	}
	if got := emptyOutputModelsForAccount(views, "missing"); len(got) != 0 {
		t.Fatalf("missing account = %#v", got)
	}
	if err := config.UpdateEmptyOutputCooldown(false, 30); err != nil {
		t.Fatal(err)
	}
	if got := emptyOutputModelViews(items); len(got) != 0 {
		t.Fatalf("disabled switch still listed %#v", got)
	}
}
