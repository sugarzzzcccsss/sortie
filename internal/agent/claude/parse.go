package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
)

// rawEvent is the intermediate representation of a Claude Code JSONL
// line. Fields are populated from JSON and then mapped to domain
// event types by the adapter.
type rawEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`

	// System/init fields.
	SessionID         string `json:"session_id,omitempty"`
	Cwd               string `json:"cwd,omitempty"`
	ClaudeCodeVersion string `json:"claude_code_version,omitempty"`

	// System/permission_denied fields.
	ToolName string `json:"tool_name,omitempty"`

	// Task lifecycle fields.
	TaskID         string              `json:"task_id,omitempty"`
	RunID          string              `json:"run_id,omitempty"`
	Status         string              `json:"status,omitempty"`
	Ambient        bool                `json:"ambient,omitempty"`
	IsBackgrounded *bool               `json:"is_backgrounded,omitempty"`
	Tasks          []rawBackgroundTask `json:"tasks,omitempty"`
	Patch          struct {
		Status         string `json:"status,omitempty"`
		IsBackgrounded *bool  `json:"is_backgrounded,omitempty"`
	} `json:"patch,omitempty"`

	// Result fields.
	Result      string `json:"result,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	DurationAPI int64  `json:"duration_api_ms,omitempty"`
	NumTurns    int    `json:"num_turns,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`

	// Usage (present in result events).
	Usage *rawUsage `json:"usage,omitempty"`

	// ModelUsage is the per-model usage breakdown on a result event,
	// present when the terminal turn touched at least one model.
	ModelUsage map[string]rawModelUsage `json:"modelUsage,omitempty"`

	// Assistant message wrapper.
	Message json.RawMessage `json:"message,omitempty"`

	// API retry fields.
	Attempt      int    `json:"attempt,omitempty"`
	MaxRetries   int    `json:"max_retries,omitempty"`
	RetryDelayMS int    `json:"retry_delay_ms,omitempty"`
	ErrorStatus  int    `json:"error_status,omitempty"`
	ErrorField   string `json:"error,omitempty"`
}

// rawUsage holds token counts from a Claude Code result event.
type rawUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
}

// rawModelUsage holds one model's token counts from a result event's
// modelUsage map.
type rawModelUsage struct {
	InputTokens              int64 `json:"inputTokens"`
	OutputTokens             int64 `json:"outputTokens"`
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
}

// rawContentBlock represents a single block inside an assistant
// message content array.
type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// toolMappings gives the kind and main-argument member of each Claude Code
// tool the adapter reports an argument for.
var toolMappings = map[string]agentcore.ToolMapping{
	"Bash":         {Kind: domain.ToolKindShell, Key: "command"},
	"Read":         {Kind: domain.ToolKindRead, Key: "file_path"},
	"Write":        {Kind: domain.ToolKindEdit, Key: "file_path"},
	"Edit":         {Kind: domain.ToolKindEdit, Key: "file_path"},
	"NotebookEdit": {Kind: domain.ToolKindEdit, Key: "notebook_path"},
	"Skill":        {Kind: domain.ToolKindSkill, Key: "skill"},
}

const mcpToolPrefix = "mcp__"

// toolCallOf maps a tool_use block's name and input to a tool call. An MCP
// tool is named {server}.{tool} and carries no kind or argument.
func toolCallOf(name string, input json.RawMessage) agentcore.ToolCall {
	if rest, ok := strings.CutPrefix(name, mcpToolPrefix); ok {
		if server, tool, ok := strings.Cut(rest, "__"); ok {
			return agentcore.ToolCall{Name: agentcore.QualifiedToolName(server, tool)}
		}
	}
	return agentcore.MapToolCall(toolMappings, name, input)
}

// rawAssistantMessage is the nested message object inside an
// assistant-type event.
type rawAssistantMessage struct {
	Content []rawContentBlock `json:"content,omitempty"`
}

// rawAssistantMessageMeta holds the message id, model, and per-request
// usage from an assistant event's message object. Claude Code repeats
// one message id across every streamed event of the same model
// request, so ID is the deduplication key.
type rawAssistantMessageMeta struct {
	ID    string    `json:"id,omitempty"`
	Model string    `json:"model,omitempty"`
	Usage *rawUsage `json:"usage,omitempty"`
}

// parseEvent parses a single JSONL line from Claude Code stdout into
// a [rawEvent]. Returns an error if JSON parsing fails.
func parseEvent(line []byte) (rawEvent, error) {
	var ev rawEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return rawEvent{}, fmt.Errorf("parse event: %w", err)
	}
	return ev, nil
}

// usageFromAssistant converts a raw usage payload from a Claude Code
// assistant event into a [domain.TokenUsage]. InputTokens includes
// prompt-cache reads and prompt-cache writes; TotalTokens is computed
// as InputTokens plus OutputTokens.
func usageFromAssistant(raw *rawUsage) domain.TokenUsage {
	if raw == nil {
		return domain.TokenUsage{}
	}
	input := raw.InputTokens + raw.CacheReadInputTokens + raw.CacheCreationInputTokens
	return domain.TokenUsage{
		InputTokens:      input,
		OutputTokens:     raw.OutputTokens,
		TotalTokens:      input + raw.OutputTokens,
		CacheReadTokens:  raw.CacheReadInputTokens,
		CacheWriteTokens: raw.CacheCreationInputTokens,
	}
}

// usageFromResult returns the usage the result event reports and its split
// by model. It prefers the modelUsage map, which is present whenever the turn
// touched at least one model and includes sub-agent activity that the
// top-level usage object excludes; it falls back to the top-level usage
// object when modelUsage is absent or empty, in which case byModel is nil.
// InputTokens includes prompt-cache reads and prompt-cache writes in both
// cases; TotalTokens is computed as InputTokens plus OutputTokens.
//
// On a process that restored its session's saved totals the figure is
// cumulative over the whole session, so callers subtract a baseline
// instead of adding it to a run total.
func usageFromResult(event rawEvent) (usage domain.TokenUsage, byModel map[string]domain.TokenUsage) {
	if len(event.ModelUsage) == 0 {
		return usageFromAssistant(event.Usage), nil
	}
	byModel = make(map[string]domain.TokenUsage, len(event.ModelUsage))
	for name, mu := range event.ModelUsage {
		figure := usageFromAssistant(&rawUsage{
			InputTokens:              mu.InputTokens,
			OutputTokens:             mu.OutputTokens,
			CacheReadInputTokens:     mu.CacheReadInputTokens,
			CacheCreationInputTokens: mu.CacheCreationInputTokens,
		})
		byModel[name] = figure
		usage = agentcore.AddUsage(usage, figure)
	}
	return usage, byModel
}

// firstRestoringRelease is the first release whose resumed process
// restores the totals its session saved.
const firstRestoringRelease = "2.1.277"

// restoresSavedTotals reports whether a process that names version on its
// init event restores saved session totals. An absent version is an
// older release; a version that is not valid semver is a newer one.
func restoresSavedTotals(version string) bool {
	if version == "" {
		return false
	}
	if !semver.IsValid("v" + version) {
		return true
	}
	return semver.Compare("v"+version, "v"+firstRestoringRelease) >= 0
}

// contentBlocks extracts the content array from an assistant message
// event. Returns nil if the message field is absent or unparseable.
func (e rawEvent) contentBlocks() []rawContentBlock {
	if len(e.Message) == 0 {
		return nil
	}
	var msg rawAssistantMessage
	if err := json.Unmarshal(e.Message, &msg); err != nil {
		return nil
	}
	return msg.Content
}

// summarizeAssistant returns the text blocks of an assistant message event
// joined by spaces, or "" when there are none. Tool calls are reported
// separately as tool_result events, so naming them here would only repeat
// them as noise in the run timeline.
func summarizeAssistant(event rawEvent) string {
	var parts []string
	for _, b := range event.contentBlocks() {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, redact.Truncate(b.Text, 200))
		}
	}
	return strings.Join(parts, " ")
}

// formatAPIRetry formats an api_retry system event into a
// human-readable notification string.
func formatAPIRetry(event rawEvent) string {
	return fmt.Sprintf("API retry attempt %d/%d (delay %dms, status %d: %s)",
		event.Attempt, event.MaxRetries, event.RetryDelayMS,
		event.ErrorStatus, event.ErrorField)
}
