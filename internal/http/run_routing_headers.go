package http

import (
	"net/http"
	"strings"
)

func extractRunLocalKey(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-GoClaw-Local-Key"))
}

func extractRunPeerKind(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-GoClaw-Peer-Kind"))
}
