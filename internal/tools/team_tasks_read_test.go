package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func TestExecute_NotificationRunBlocked(t *testing.T) {
	mb, tool, _, _, ctx := newTestTeamSetup()
	_ = mb

	notifCtx := WithRunKind(ctx, RunKindNotification)

	tests := []struct {
		action  string
		wantErr bool
	}{
		{"list", false},
		{"get", false},
		{"search", false},
		{"create", true},
		{"claim", true},
		{"complete", true},
		{"cancel", true},
		{"approve", true},
		{"reject", true},
		{"comment", true},
		{"progress", true},
		{"update", true},
		{"retry", true},
	}

	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			result := tool.Execute(notifCtx, map[string]any{"action": tt.action})
			if tt.wantErr && !result.IsError {
				t.Errorf("expected error for action %q during notification run", tt.action)
			}
			if !tt.wantErr && result.IsError {
				if strings.Contains(result.ForLLM, "notification run") {
					t.Errorf("action %q should be allowed during notification run, got: %s", tt.action, result.ForLLM)
				}
			}
		})
	}
}

func TestExecute_PolicyBlocked(t *testing.T) {
	mb, _, _, _, ctx := newTestTeamSetup()

	liteTool := NewTeamTasksTool(mb, LiteTeamPolicy{})

	blocked := []string{"comment", "review", "approve", "reject", "attach", "ask_user", "clear_ask_user"}
	for _, action := range blocked {
		t.Run(action, func(t *testing.T) {
			result := liteTool.Execute(ctx, map[string]any{"action": action})
			if !result.IsError {
				t.Errorf("expected policy block for action %q in lite edition", action)
			}
			if !strings.Contains(result.ForLLM, "not available in this edition") {
				t.Errorf("expected edition error, got: %s", result.ForLLM)
			}
		})
	}
}

func TestExecute_UnknownAction(t *testing.T) {
	_, tool, _, _, ctx := newTestTeamSetup()

	result := tool.Execute(ctx, map[string]any{"action": "nonexistent"})
	if !result.IsError {
		t.Fatal("expected error for unknown action")
	}
	if !strings.Contains(result.ForLLM, "unknown action") {
		t.Errorf("expected 'unknown action' error, got: %s", result.ForLLM)
	}
}

func TestList_FiltersByUserForExternalChannels(t *testing.T) {
	_, tool, _, _, ctx := newTestTeamSetup()

	extCtx := WithToolChannel(ctx, "telegram")
	extCtx = store.WithUserID(extCtx, "user123")

	result := tool.Execute(extCtx, map[string]any{"action": "list"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
}

func TestList_NoFilterForTeammate(t *testing.T) {
	_, tool, _, _, ctx := newTestTeamSetup()

	teamCtx := WithToolChannel(ctx, ChannelTeammate)

	result := tool.Execute(teamCtx, map[string]any{"action": "list"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
}

func TestSearch_SatisfiesCreateGate(t *testing.T) {
	_, tool, _, _, ctx := newTestTeamSetup()

	ptd := NewPendingTeamDispatch()
	ctx = WithPendingTeamDispatch(ctx, ptd)

	if ptd.HasListed() {
		t.Fatal("expected HasListed=false before search")
	}

	tool.Execute(ctx, map[string]any{"action": "search", "query": "test"})

	if !ptd.HasListed() {
		t.Error("expected HasListed=true after search")
	}
}

func TestSearch_ParallelCallsShareCreateGateLock(t *testing.T) {
	_, tool, _, _, ctx := newTestTeamSetup()
	chatID := "parallel-search-lock"
	ctx = WithToolChatID(ctx, chatID)

	ptd := NewPendingTeamDispatch()
	ctx = WithPendingTeamDispatch(ctx, ptd)

	lock := getTeamCreateLock(testTeamID.String(), chatID)
	lock.Lock()
	prelocked := true
	defer func() {
		if prelocked {
			lock.Unlock()
		}
		ptd.ReleaseTeamLock()
	}()

	started := make(chan struct{}, 2)
	done := make(chan *Result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			started <- struct{}{}
			done <- tool.Execute(ctx, map[string]any{"action": "search", "query": "test"})
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for parallel searches to start")
		}
	}

	time.Sleep(50 * time.Millisecond)
	lock.Unlock()
	prelocked = false

	for i := 0; i < 2; i++ {
		select {
		case result := <-done:
			if result.IsError {
				t.Fatalf("unexpected search error: %s", result.ForLLM)
			}
		case <-time.After(time.Second):
			t.Fatal("parallel search calls deadlocked on the create gate lock")
		}
	}

	if !ptd.HasListed() {
		t.Fatal("expected parallel search to satisfy create gate")
	}
}

func TestGet_CrossTeamBlocked(t *testing.T) {
	mb, tool, _, _, ctx := newTestTeamSetup()

	otherTeamID := uuid.New()
	taskID := uuid.New()
	mb.taskStore.mu.Lock()
	mb.taskStore.tasks[taskID] = &store.TeamTaskData{
		BaseModel: store.BaseModel{ID: taskID, CreatedAt: time.Now(), UpdatedAt: time.Now()},
		TeamID:    otherTeamID,
		Subject:   "Other team task",
		Status:    store.TeamTaskStatusPending,
	}
	mb.taskStore.mu.Unlock()

	result := tool.Execute(ctx, map[string]any{"action": "get", "task_id": taskID.String()})
	if !result.IsError {
		t.Fatal("expected error for cross-team get")
	}
	if !strings.Contains(result.ForLLM, "does not belong") {
		t.Errorf("expected team ownership error, got: %s", result.ForLLM)
	}
}
