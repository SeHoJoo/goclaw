package http

import "testing"

func TestResolveChatScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		localKey string
		peerKind string
		userID   string
		want     string
	}{
		{name: "local key wins", localKey: " channel:topic:42 ", peerKind: "direct", userID: "user@example.com", want: "channel:topic:42"},
		{name: "direct request uses user", peerKind: "direct", userID: " user@example.com ", want: "user@example.com"},
		{name: "group without local key uses api", peerKind: "group", userID: "user@example.com", want: "api"},
		{name: "missing routing metadata uses api", want: "api"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveChatScope(tt.localKey, tt.peerKind, tt.userID); got != tt.want {
				t.Fatalf("resolveChatScope(%q, %q, %q) = %q, want %q", tt.localKey, tt.peerKind, tt.userID, got, tt.want)
			}
		})
	}
}
