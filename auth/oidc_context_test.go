package auth

import (
	"context"
	"errors"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRefreshTokenContextCancelsOutboundRequest(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	oldTokenURL := GetOIDCTokenURLForTest()
	SetOIDCTokenURLForTest(func(string) string { return server.URL })
	t.Cleanup(func() { SetOIDCTokenURLForTest(oldTokenURL) })
	oldClient := SetGlobalAuthClientForTest(server.Client())
	t.Cleanup(func() { SetGlobalAuthClientForTest(oldClient) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, _, err := RefreshTokenContext(ctx, &config.Account{
			AuthMethod:   "idc",
			RefreshToken: "refresh",
			ClientID:     "client",
			ClientSecret: "secret",
			Region:       "us-east-1",
		})
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("refresh error=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh request ignored cancellation")
	}
}
