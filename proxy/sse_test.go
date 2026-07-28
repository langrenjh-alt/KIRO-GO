package proxy

import "testing"

func TestSplitBeforePartialTag(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		tag         string
		wantReady   string
		wantPending string
	}{
		{name: "ordinary text", text: "hello", tag: thinkingOpenTag, wantReady: "hello"},
		{name: "opening tag prefix", text: "hello<thi", tag: thinkingOpenTag, wantReady: "hello", wantPending: "<thi"},
		{name: "closing tag prefix", text: "reasoning</think", tag: thinkingCloseTag, wantReady: "reasoning", wantPending: "</think"},
		{name: "single prefix byte", text: "answer<", tag: thinkingOpenTag, wantReady: "answer", wantPending: "<"},
		{name: "false prefix", text: "value<thx", tag: thinkingOpenTag, wantReady: "value<thx"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, pending := splitBeforePartialTag(tt.text, tt.tag)
			if ready != tt.wantReady || pending != tt.wantPending {
				t.Fatalf("splitBeforePartialTag(%q, %q) = (%q, %q), want (%q, %q)",
					tt.text, tt.tag, ready, pending, tt.wantReady, tt.wantPending)
			}
		})
	}
}
