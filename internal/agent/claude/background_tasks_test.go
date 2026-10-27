package claude

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestRunTurn_UnfinishedBackgroundTasks(t *testing.T) {
	t.Parallel()

	const started = `{"type":"system","subtype":"task_started","task_id":"task-1","task_type":"local_agent","is_backgrounded":true}` + "\n"
	const completed = `{"type":"system","subtype":"task_notification","task_id":"task-1","status":"completed"}` + "\n"
	const result = `{"type":"result","subtype":"success","result":"Done","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}` + "\n"

	tests := []struct {
		name      string
		stream    string
		wantCount int
	}{
		{name: "unfinished task", stream: started, wantCount: 1},
		{name: "completed task", stream: started + completed},
		{name: "no task", stream: ""},
		{name: "duplicate start", stream: started + started, wantCount: 1},
		{name: "failed task", stream: started + strings.ReplaceAll(completed, "completed", "failed")},
		{name: "stopped task", stream: started + strings.ReplaceAll(completed, "completed", "stopped")},
		{name: "unknown status", stream: started + strings.ReplaceAll(completed, "completed", "future_status"), wantCount: 1},
		{name: "unknown completion", stream: started + strings.ReplaceAll(completed, "task-1", "unseen"), wantCount: 1},
		{name: "missing identity", stream: strings.ReplaceAll(started, `"task-1"`, `""`)},
		{name: "old runtime", stream: `{"type":"system","subtype":"task_started","task_id":"task-1"}` + "\n", wantCount: 1},
		{name: "foreground task", stream: strings.ReplaceAll(started, `"is_backgrounded":true`, `"is_backgrounded":false`)},
		{name: "ambient task", stream: strings.ReplaceAll(started, `"is_backgrounded":true`, `"ambient":true`)},
		{name: "partial completion", stream: started + strings.ReplaceAll(started, "task-1", "task-2") + completed, wantCount: 1},
		{name: "two unfinished tasks", stream: started + strings.ReplaceAll(started, "task-1", "task-2"), wantCount: 2},
		{name: "completed update", stream: started + `{"type":"system","subtype":"task_updated","task_id":"task-1","patch":{"status":"completed"}}` + "\n"},
		{name: "failed update", stream: started + `{"type":"system","subtype":"task_updated","task_id":"task-1","patch":{"status":"failed"}}` + "\n"},
		{name: "killed update", stream: started + `{"type":"system","subtype":"task_updated","task_id":"task-1","patch":{"status":"killed"}}` + "\n"},
		{name: "foreground moved to background", stream: strings.ReplaceAll(started, `"is_backgrounded":true`, `"is_backgrounded":false`) + `{"type":"system","subtype":"task_updated","task_id":"task-1","patch":{"is_backgrounded":true}}` + "\n", wantCount: 1},
		{name: "empty snapshot replaces stale start", stream: started + `{"type":"system","subtype":"background_tasks_changed","tasks":[]}` + "\n"},
		{name: "incomplete snapshot preserves evidence", stream: started + `{"type":"system","subtype":"background_tasks_changed"}` + "\n", wantCount: 1},
		{name: "snapshot recovers missed start", stream: `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"task-1","task_type":"local_bash"},{"task_id":"watcher","ambient":true}]}` + "\n", wantCount: 1},
		{name: "snapshot excludes ambient work", stream: started + `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"watcher","ambient":true}]}` + "\n"},
		{name: "old run completion cannot finish resumed task", stream: strings.ReplaceAll(started, `"task_id":`, `"run_id":"new-run","task_id":`) + strings.ReplaceAll(completed, `"task_id":`, `"run_id":"old-run","task_id":`), wantCount: 1},
		{name: "matching run completion", stream: strings.ReplaceAll(started, `"task_id":`, `"run_id":"new-run","task_id":`) + strings.ReplaceAll(completed, `"task_id":`, `"run_id":"new-run","task_id":`)},
		{name: "completion without run id", stream: strings.ReplaceAll(started, `"task_id":`, `"run_id":"new-run","task_id":`) + completed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			adapter, err := NewClaudeCodeAdapter()
			if err != nil {
				t.Fatalf("NewClaudeCodeAdapter() error = %v", err)
			}
			session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath: dir,
				AgentConfig: domain.AgentConfig{
					Command: fakeClaude(t, dir, agenttest.Output{Stdout: tt.stream + result}),
				},
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}

			var events []domain.AgentEvent
			turn, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
				Prompt: "delegate work",
				OnEvent: func(event domain.AgentEvent) {
					events = append(events, event)
				},
			})
			if err != nil {
				t.Fatalf("RunTurn() error = %v", err)
			}
			if turn.ExitReason != domain.EventTurnCompleted {
				t.Errorf("RunTurn().ExitReason = %q, want %q", turn.ExitReason, domain.EventTurnCompleted)
			}

			var notices []string
			for _, event := range events {
				if event.Type == domain.EventNotification && strings.Contains(event.Message, "background task") {
					notices = append(notices, event.Message)
				}
			}
			want := 0
			if tt.wantCount > 0 {
				want = 1
			}
			if len(notices) != want {
				t.Fatalf("RunTurn(%s) background-task notices = %v, want %d", tt.name, notices, want)
			}
			if tt.wantCount > 0 && (!strings.Contains(notices[0], fmt.Sprintf("%d background task", tt.wantCount)) || !strings.Contains(notices[0], "stopped")) {
				t.Errorf("RunTurn(%s) notice = %q, want count and stopped disposition", tt.name, notices[0])
			}
			if events[len(events)-1].Type != domain.EventTurnCompleted {
				t.Errorf("RunTurn(%s) last event = %q, want %q", tt.name, events[len(events)-1].Type, domain.EventTurnCompleted)
			}
		})
	}
}

func TestRunTurn_BackgroundNoticeOnInterruptedTurn(t *testing.T) {
	t.Parallel()

	const started = `{"type":"system","subtype":"task_started","task_id":"task-1","is_backgrounded":true}` + "\n"
	tests := []struct {
		name       string
		out        agenttest.Output
		cancel     bool
		wantReason domain.AgentEventType
	}{
		{name: "process exits", out: agenttest.Output{Stdout: started, ExitCode: 7}, wantReason: domain.EventTurnFailed},
		{name: "cancelled", out: agenttest.Output{Stdout: started, Hang: true}, cancel: true, wantReason: domain.EventTurnCancelled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			adapter, err := NewClaudeCodeAdapter()
			if err != nil {
				t.Fatalf("NewClaudeCodeAdapter() error = %v", err)
			}
			session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath: dir,
				AgentConfig:   domain.AgentConfig{Command: fakeClaude(t, dir, tt.out)},
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var events []domain.AgentEvent
			turn, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
				Prompt: "delegate work",
				OnEvent: func(event domain.AgentEvent) {
					events = append(events, event)
					if tt.cancel && event.Type == domain.EventNotification && event.Message == "" {
						cancel()
					}
				},
			})
			if err == nil {
				t.Fatal("RunTurn() error = nil, want interrupted-turn error")
			}
			if turn.ExitReason != tt.wantReason {
				t.Errorf("RunTurn().ExitReason = %q, want %q", turn.ExitReason, tt.wantReason)
			}
			if len(events) < 2 || events[len(events)-1].Type != tt.wantReason {
				t.Fatalf("RunTurn() events = %v, want notice before %q", events, tt.wantReason)
			}
			notice := events[len(events)-2]
			if notice.Type != domain.EventNotification || !strings.Contains(notice.Message, "1 background task") {
				t.Errorf("RunTurn() penultimate event = %v, want one unfinished background task", notice)
			}
		})
	}
}

func TestRunTurn_BackgroundTasksResetBetweenTurns(t *testing.T) {
	t.Parallel()

	const started = `{"type":"system","subtype":"task_started","task_id":"task-1","is_backgrounded":true}` + "\n"
	const result = `{"type":"result","subtype":"success","result":"Done","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}` + "\n"
	dir := t.TempDir()
	adapter, err := NewClaudeCodeAdapter()
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter() error = %v", err)
	}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: dir,
		AgentConfig: domain.AgentConfig{
			Command: fakeSequentialClaude(t, dir, agenttest.Output{Stdout: started + result}, agenttest.Output{Stdout: result}),
		},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	for turn, want := range []int{1, 0} {
		notices := 0
		_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt: "delegate work",
			OnEvent: func(event domain.AgentEvent) {
				if event.Type == domain.EventNotification && strings.Contains(event.Message, "background task") {
					notices++
				}
			},
		})
		if err != nil {
			t.Fatalf("RunTurn(%d) error = %v", turn, err)
		}
		if notices != want {
			t.Errorf("RunTurn(%d) notices = %d, want %d", turn, notices, want)
		}
	}
}
