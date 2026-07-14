package http

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/permissions"
	"github.com/nextlevelbuilder/goclaw/internal/sessions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// ChatCompletionsHandler handles POST /v1/chat/completions (OpenAI-compatible).
type ChatCompletionsHandler struct {
	agents      *agent.Router
	sessions    store.SessionStore
	isManaged   bool
	rateLimiter func(string) bool // rate limit check: key → allowed (nil = no limit)
	postTurn    tools.PostTurnProcessor
}

// SetPostTurnProcessor sets the post-turn processor for team task dispatch.
func (h *ChatCompletionsHandler) SetPostTurnProcessor(pt tools.PostTurnProcessor) {
	h.postTurn = pt
}

// NewChatCompletionsHandler creates a handler for the chat completions endpoint.
func NewChatCompletionsHandler(agents *agent.Router, sess store.SessionStore, isManaged bool) *ChatCompletionsHandler {
	return &ChatCompletionsHandler{
		agents:    agents,
		sessions:  sess,
		isManaged: isManaged,
	}
}

// SetRateLimiter sets the rate limiter function for HTTP requests.
func (h *ChatCompletionsHandler) SetRateLimiter(fn func(string) bool) {
	h.rateLimiter = fn
}

type chatCompletionsRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	User     string        `json:"user,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

type chatCompletionsResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int          `json:"index"`
	Message      *chatMessage `json:"message,omitempty"`
	Delta        *chatMessage `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (h *ChatCompletionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)

	if r.Method != http.MethodPost {
		http.Error(w, i18n.T(locale, i18n.MsgMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	// Auth + RBAC check (gateway token or API key, operator required for POST)
	auth := resolveAuth(r)
	if !auth.Authenticated {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"invalid_request_error"}}`, i18n.T(locale, i18n.MsgInvalidAuth)), http.StatusUnauthorized)
		return
	}
	if !permissions.HasMinRole(auth.Role, permissions.RoleOperator) {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"invalid_request_error"}}`, i18n.T(locale, i18n.MsgPermissionDenied, "/v1/chat/completions")), http.StatusForbidden)
		return
	}

	// Inject tenant, role, user, and locale into context for downstream stores/tools.
	r = r.WithContext(enrichContext(r.Context(), r, auth))

	// Rate limit check (per IP or bearer token)
	if h.rateLimiter != nil {
		key := r.RemoteAddr
		if token := extractBearerToken(r); token != "" {
			key = "token:" + token
		}
		if !h.rateLimiter(key) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"rate_limit_error"}}`, i18n.T(locale, i18n.MsgRateLimitExceeded)), http.StatusTooManyRequests)
			return
		}
	}

	// Limit request body size to prevent DoS
	const maxRequestBodySize = 1 << 20 // 1MB
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var req chatCompletionsRequest
	if !bindJSON(w, r, locale, &req) {
		return
	}

	if len(req.Messages) == 0 {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, i18n.T(locale, i18n.MsgMsgsRequired)), http.StatusBadRequest)
		return
	}

	agentID := extractAgentID(r, req.Model)
	userID := store.UserIDFromContext(r.Context()) // resolved by enrichContext (respects API key owner binding)
	if h.isManaged && userID == "" {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, i18n.T(locale, i18n.MsgUserIDHeader)), http.StatusBadRequest)
		return
	}

	loop, err := h.agents.Get(r.Context(), agentID)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, i18n.T(locale, i18n.MsgNotFound, "agent", agentID)), http.StatusNotFound)
		return
	}

	// Extract the last user message
	var lastMessage string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastMessage = req.Messages[i].Content
			break
		}
	}
	if lastMessage == "" {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, i18n.T(locale, i18n.MsgNoUserMessage)), http.StatusBadRequest)
		return
	}

	runID := uuid.NewString()
	sessionKey := extractSessionKey(r, agentID, userID, runID)
	localKey := extractRunLocalKey(r)
	peerKind := extractRunPeerKind(r)

	slog.Info("chat completions request", "agent", agentID, "stream", req.Stream, "user", userID)

	if req.Stream {
		h.handleStream(w, r, loop, runID, sessionKey, lastMessage, req.Model, userID, localKey, peerKind)
	} else {
		h.handleNonStream(w, r, loop, runID, sessionKey, lastMessage, req.Model, userID, localKey, peerKind)
	}
}

func extractSessionKey(r *http.Request, agentID, userID, runID string) string {
	for _, header := range []string{"X-GoClaw-Session-Key", "X-Session-Key"} {
		if key := strings.TrimSpace(r.Header.Get(header)); key != "" {
			return key
		}
	}

	// Include userID in session key for multi-tenant isolation when the caller
	// did not provide an explicit session key.
	sessionSuffix := "http-" + runID[:8]
	if userID != "" {
		sessionSuffix = "http-" + userID + "-" + runID[:8]
	}
	return sessions.SessionKey(agentID, sessionSuffix)
}

func resolveChatScope(localKey, peerKind, userID string) string {
	if localKey = strings.TrimSpace(localKey); localKey != "" {
		return localKey
	}
	if peerKind == "direct" && strings.TrimSpace(userID) != "" {
		return strings.TrimSpace(userID)
	}
	return "api"
}

func (h *ChatCompletionsHandler) handleNonStream(w http.ResponseWriter, r *http.Request, loop agent.Agent, runID, sessionKey, message, model, userID, localKey, peerKind string) {
	ctx, drainTeamDispatch := tools.InjectTeamDispatch(r.Context(), h.postTurn)
	defer drainTeamDispatch()
	chatScope := resolveChatScope(localKey, peerKind, userID)

	result, err := loop.Run(ctx, agent.RunRequest{
		SessionKey:       sessionKey,
		Message:          message,
		Channel:          "http",
		ChatID:           chatScope,
		RunID:            runID,
		UserID:           userID,
		LocalKey:         localKey,
		PeerKind:         peerKind,
		WorkspaceChannel: "http",
		WorkspaceChatID:  chatScope,
		Stream:           false,
	})

	if err != nil {
		locale := store.LocaleFromContext(r.Context())
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, i18n.T(locale, i18n.MsgInternalError, err.Error())), http.StatusInternalServerError)
		return
	}

	resp := chatCompletionsResponse{
		ID:      "chatcmpl-" + runID[:8],
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      &chatMessage{Role: "assistant", Content: SignFileURLs(result.Content, FileSigningKey())},
			FinishReason: "stop",
		}},
	}

	if result.Usage != nil {
		resp.Usage = &chatUsage{
			PromptTokens:     result.Usage.PromptTokens,
			CompletionTokens: result.Usage.CompletionTokens,
			TotalTokens:      result.Usage.TotalTokens,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *ChatCompletionsHandler) handleStream(w http.ResponseWriter, r *http.Request, loop agent.Agent, runID, sessionKey, message, model, userID, localKey, peerKind string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		locale := store.LocaleFromContext(r.Context())
		http.Error(w, i18n.T(locale, i18n.MsgStreamingNotSupported), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	completionID := "chatcmpl-" + runID[:8]

	// Send initial role chunk
	writeSSEChunk(w, flusher, completionID, model, &chatMessage{Role: "assistant"}, "")

	ctx, drainTeamDispatch := tools.InjectTeamDispatch(r.Context(), h.postTurn)
	defer drainTeamDispatch()
	chatScope := resolveChatScope(localKey, peerKind, userID)

	result, err := loop.Run(ctx, agent.RunRequest{
		SessionKey:       sessionKey,
		Message:          message,
		Channel:          "http",
		ChatID:           chatScope,
		RunID:            runID,
		UserID:           userID,
		LocalKey:         localKey,
		PeerKind:         peerKind,
		WorkspaceChannel: "http",
		WorkspaceChatID:  chatScope,
		Stream:           true,
	})

	if err != nil {
		writeSSEChunk(w, flusher, completionID, model, &chatMessage{Content: "Error: " + err.Error()}, "stop")
	} else {
		// Send content chunk
		writeSSEChunk(w, flusher, completionID, model, &chatMessage{Content: SignFileURLs(result.Content, FileSigningKey())}, "stop")
	}

	// Send [DONE]
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeSSEChunk(w http.ResponseWriter, flusher http.Flusher, id, model string, delta *chatMessage, finishReason string) {
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": nilIfEmpty(finishReason),
		}},
	}

	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
