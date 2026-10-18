package proxy

import (
	"errors"
	"io"
	"kiro-go/config"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMarshalUpstreamJSONKeepsAngleBrackets(t *testing.T) {
	body, err := marshalUpstreamJSON(map[string]string{"content": "<thinking_mode>&ok"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, `\u003c`) || strings.Contains(text, `\u003e`) || strings.Contains(text, `\u0026`) {
		t.Fatalf("upstream JSON escaped HTML: %s", text)
	}
	if strings.Contains(text, "\n") {
		t.Fatalf("upstream JSON gained a trailing newline: %q", text)
	}
	if !strings.Contains(text, "<thinking_mode>&ok") {
		t.Fatalf("upstream JSON lost raw characters: %s", text)
	}
}

func TestAccountUpstreamClientsDoNotShareTransport(t *testing.T) {
	InitKiroHttpClient("")
	t.Cleanup(func() { InitKiroHttpClient("") })

	first := kiroUpstreamClient(&config.Account{ID: "acct-a"}, false)
	second := kiroUpstreamClient(&config.Account{ID: "acct-b"}, false)
	firstREST := kiroUpstreamClient(&config.Account{ID: "acct-a"}, true)
	if first == nil || second == nil || first == second || first.Transport == second.Transport {
		t.Fatal("two accounts shared one upstream connection pool")
	}
	if first.Transport != firstREST.Transport {
		t.Fatal("one account used different transports for streaming and REST")
	}
	again := kiroUpstreamClient(&config.Account{ID: "acct-a"}, false)
	if again != first {
		t.Fatal("the same account allocated a second streaming client")
	}

	streamStub := &http.Client{}
	kiroHttpStore.Store(streamStub)
	if got := kiroUpstreamClient(&config.Account{ID: "acct-a"}, false); got != streamStub {
		t.Fatal("a replaced streaming client was not used")
	}
	restStub := &http.Client{}
	kiroRestHttpStore.Store(restStub)
	if got := kiroUpstreamClient(&config.Account{ID: "acct-a"}, true); got != restStub {
		t.Fatal("a replaced REST client was not used")
	}
}

func TestCallKiroAPIKeepsRawJSONAndCountsSDKRetries(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	var attempts []string
	var bodies []string
	kiroHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts = append(attempts, req.Header.Get("Amz-Sdk-Request"))
			payload, readErr := io.ReadAll(req.Body)
			if readErr != nil {
				t.Errorf("read body: %v", readErr)
			}
			bodies = append(bodies, string(payload))
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Body:       io.NopCloser(strings.NewReader(`{"message":"thinking_signature_invalid"}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	payload := &KiroPayload{}
	payload.HistoryMode = kiroHistoryModeNativeActive
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "<thinking_mode>enabled</thinking_mode>"
	payload.ConversationState.History = []KiroHistoryMessage{{
		AssistantResponseMessage: &KiroAssistantResponseMessage{
			Content: "previous",
			ReasoningContent: &KiroReasoningContent{
				ReasoningText: &KiroReasoningText{Text: "reason", Signature: "sig"},
			},
		},
	}}
	account := &config.Account{
		ID:          "sdk-attempt",
		AuthMethod:  "social",
		AccessToken: "token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:profile/sdk-attempt",
		MachineId:   "machine-123",
	}
	err := CallKiroAPI(account, payload, nil)
	var httpErr *KiroHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("CallKiroAPI error=%v, want the second HTTP 400", err)
	}
	if len(attempts) != 2 || attempts[0] != "attempt=1; max=3" || attempts[1] != "attempt=2; max=3" {
		t.Fatalf("SDK attempts = %#v", attempts)
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies = %d", len(bodies))
	}
	if strings.Contains(bodies[0], `\u003c`) || !strings.Contains(bodies[0], "<thinking_mode>enabled</thinking_mode>") {
		t.Fatalf("first body was not raw JSON: %s", bodies[0])
	}
	if strings.Contains(bodies[0], "KiroAPIProxy") || strings.Contains(bodies[0], "Go-http-client") {
		t.Fatalf("body identified the proxy: %s", bodies[0])
	}
}

func TestRefreshAccountSpacingIsBounded(t *testing.T) {
	if refreshAccountSpacing(0) != 0 || refreshAccountSpacing(1) != 0 {
		t.Fatal("a single enabled account must not wait")
	}
	if got := refreshAccountSpacing(2); got != 15*time.Second {
		t.Fatalf("small pool spacing = %s", got)
	}
	if got := refreshAccountSpacing(100000); got != 200*time.Millisecond {
		t.Fatalf("large pool spacing = %s", got)
	}
}

func TestMergedToolSchemaDoesNotDescribeTheProxy(t *testing.T) {
	got := ensureObjectSchema(map[string]interface{}{
		"anyOf": []interface{}{
			map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"a": map[string]interface{}{"type": "string"}},
			},
		},
	}).(map[string]interface{})
	if _, ok := got["properties"].(map[string]interface{})["a"]; !ok {
		t.Fatalf("merged property disappeared: %#v", got)
	}
	if desc, _ := got["description"].(string); strings.Contains(desc, "upstream") || strings.Contains(desc, "oneOf") {
		t.Fatalf("schema description identifies the rewrite: %q", desc)
	}
}

func TestUpstreamConversationIDsAreAccountScopedV4(t *testing.T) {
	payload := &KiroPayload{}
	payload.HistoryMode = kiroHistoryModeNativeActive
	payload.ConversationState.ConversationID = "conversation-123"
	payload.ConversationState.History = []KiroHistoryMessage{{
		AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "hi", MessageID: "old-message"},
	}}

	bindUpstreamConversation(payload, &config.Account{ID: "account-a"})
	first := payload.ConversationState.ConversationID
	firstMessage := payload.ConversationState.History[0].AssistantResponseMessage.MessageID
	bindUpstreamConversation(payload, &config.Account{ID: "account-a"})
	if payload.ConversationState.ConversationID != first || payload.ConversationState.History[0].AssistantResponseMessage.MessageID != firstMessage {
		t.Fatal("the same account changed conversation or message id")
	}
	bindUpstreamConversation(payload, &config.Account{ID: "account-b"})
	second := payload.ConversationState.ConversationID
	if second == first || second == "conversation-123" || payload.ConversationState.History[0].AssistantResponseMessage.MessageID == firstMessage {
		t.Fatalf("account b reused account a ids: conversation=%s message=%s", second, payload.ConversationState.History[0].AssistantResponseMessage.MessageID)
	}
	if uuidVersionNibble(first) != '4' || uuidVersionNibble(second) != '4' || uuidVersionNibble(firstMessage) != '4' {
		t.Fatalf("ids are not version 4: %s %s %s", first, second, firstMessage)
	}

	payload.ConversationState.ConversationID = "fresh-conversation"
	bindUpstreamConversation(payload, &config.Account{ID: "account-b"})
	if payload.ConversationState.ConversationID == second {
		t.Fatal("a replaced conversation id was ignored")
	}
	if uuidVersionNibble(payload.ConversationState.ConversationID) != '4' {
		t.Fatalf("replacement id = %s", payload.ConversationState.ConversationID)
	}
}

func uuidVersionNibble(id string) byte {
	parts := strings.Split(id, "-")
	if len(parts) != 5 || len(parts[2]) < 1 {
		return 0
	}
	return parts[2][0]
}

func TestWaitBetweenAccountRefreshesStops(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	if waitBetweenAccountRefreshes(stop, time.Hour) {
		t.Fatal("a closed stop channel must interrupt the refresh gap")
	}
	if !waitBetweenAccountRefreshes(nil, time.Hour) {
		t.Fatal("a nil stop channel keeps the existing immediate refresh")
	}
}
