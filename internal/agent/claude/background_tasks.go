package claude

import (
	"fmt"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
)

// rawBackgroundTask is one member of the runtime's live background set.
type rawBackgroundTask struct {
	TaskID  string `json:"task_id"`
	RunID   string `json:"run_id,omitempty"`
	Ambient bool   `json:"ambient,omitempty"`
}

// backgroundTask retains a task's current run and whether it is backgrounded.
type backgroundTask struct {
	runID        string
	backgrounded bool
}

// observeBackgroundTasks tracks the runtime's live work, replacing the set
// on a snapshot so missed lifecycle events cannot leave stale tasks behind.
func (s *sessionState) observeBackgroundTasks(event rawEvent) {
	if event.Subtype == "background_tasks_changed" {
		if event.Tasks == nil {
			return
		}
		clear(s.backgroundTasks)
		for _, task := range event.Tasks {
			if task.TaskID != "" && !task.Ambient {
				s.backgroundTasks[task.TaskID] = backgroundTask{runID: task.RunID, backgrounded: true}
			}
		}
		return
	}
	if event.TaskID == "" {
		return
	}
	if event.Subtype == "task_started" {
		if event.Ambient {
			delete(s.backgroundTasks, event.TaskID)
			return
		}
		s.backgroundTasks[event.TaskID] = backgroundTask{
			runID:        event.RunID,
			backgrounded: event.IsBackgrounded == nil || *event.IsBackgrounded,
		}
		return
	}
	task, exists := s.backgroundTasks[event.TaskID]
	if !exists || (event.RunID != "" && task.runID != "" && event.RunID != task.runID) {
		return
	}
	switch event.Subtype {
	case "task_notification":
		switch event.Status {
		case "completed", "failed", "stopped":
			delete(s.backgroundTasks, event.TaskID)
		}
	case "task_updated":
		switch event.Patch.Status {
		case "completed", "failed", "killed":
			delete(s.backgroundTasks, event.TaskID)
		default:
			if event.Patch.IsBackgrounded != nil {
				task.backgrounded = *event.Patch.IsBackgrounded
				s.backgroundTasks[event.TaskID] = task
			}
		}
	}
}

// reportBackgroundTasks emits one notice before the turn's terminal event.
// It reports only a count, never a task description or captured output.
func (s *sessionState) reportBackgroundTasks(emit func(domain.AgentEvent)) {
	count := 0
	for _, task := range s.backgroundTasks {
		if task.backgrounded {
			count++
		}
	}
	if count > 0 {
		noun := "tasks"
		if count == 1 {
			noun = "task"
		}
		agentcore.EmitNotification(emit, fmt.Sprintf("Claude Code ended the turn with %d background %s still running; they stopped with the session.", count, noun))
	}
	clear(s.backgroundTasks)
}
