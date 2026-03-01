package backend

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/ihavespoons/transitive/internal/protocol"
)

// ClaudeBackend implements Backend for Claude Code CLI.
type ClaudeBackend struct {
	mu        sync.RWMutex
	path      string
	available bool
	providers []ProviderInfo
}

// NewClaudeBackend creates a new Claude Code backend.
func NewClaudeBackend(claudePath string) *ClaudeBackend {
	b := &ClaudeBackend{
		path: claudePath,
		providers: []ProviderInfo{
			{ID: "anthropic", Name: "Anthropic", Models: []string{"sonnet", "opus", "haiku"}},
		},
	}
	b.checkAvailability()
	return b
}

func (b *ClaudeBackend) checkAvailability() {
	_, err := exec.LookPath(b.path)
	b.mu.Lock()
	b.available = err == nil
	b.mu.Unlock()
}

func (b *ClaudeBackend) ID() string { return "claude" }

func (b *ClaudeBackend) Info() BackendInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return BackendInfo{
		ID:        "claude",
		Name:      "Claude Code",
		Available: b.available,
		Providers: b.providers,
	}
}

func (b *ClaudeBackend) BinaryPath() string { return b.path }

func (b *ClaudeBackend) SupportsHooks() bool { return true }

func (b *ClaudeBackend) RefreshProviders() error {
	b.checkAvailability()
	return nil
}

// RunPrompt spawns a claude CLI process and streams output via the emitter.
func (b *ClaudeBackend) RunPrompt(opts RunOpts) error {
	args := []string{
		"-p", opts.Prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	if opts.HasSession {
		args = append(args, "--resume", opts.SessionID)
	} else {
		args = append(args, "--session-id", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	args = append(args, "--dangerously-skip-permissions")

	cmd := exec.CommandContext(opts.Ctx, b.path, args...)
	cmd.Dir = opts.Cwd

	// Clear Claude Code env vars to avoid nested session detection.
	env := os.Environ()
	filtered := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, "CLAUDECODE=") && !strings.HasPrefix(e, "CLAUDE_CODE_ENTRYPOINT=") {
			filtered = append(filtered, e)
		}
	}
	cmd.Env = filtered

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	log.Printf("[claude %s] starting: claude -p %q --resume %s", opts.InstanceID, opts.Prompt, opts.SessionID)
	if err := cmd.Start(); err != nil {
		opts.Emitter(protocol.TypeNotification, protocol.Notification{
			InstanceID: opts.InstanceID,
			Type:       "error",
			Message:    fmt.Sprintf("failed to start claude: %v", err),
		})
		return fmt.Errorf("start failed: %w", err)
	}

	// Read stderr in background.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[claude %s stderr] %s", opts.InstanceID, scanner.Text())
		}
	}()

	// Stream stdout.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		log.Printf("[claude %s stdout] %s", opts.InstanceID, string(line))

		var evt streamEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			log.Printf("[claude %s] invalid JSON line: %v", opts.InstanceID, err)
			continue
		}
		handleStreamEvent(opts, &evt)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[claude %s] stream read error: %v", opts.InstanceID, err)
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("[claude %s] process exited: %v", opts.InstanceID, err)
	} else {
		log.Printf("[claude %s] process exited cleanly", opts.InstanceID)
	}
	return nil
}

// --- Stream event types from Claude CLI ---

type streamEvent struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`

	Message *assistantMessage `json:"message,omitempty"`

	Result            string             `json:"result,omitempty"`
	CostUSD           float64            `json:"cost_usd,omitempty"`
	Duration          float64            `json:"duration_ms,omitempty"`
	SessionID         string             `json:"session_id,omitempty"`
	IsError           bool               `json:"is_error,omitempty"`
	PermissionDenials []permissionDenial `json:"permission_denials,omitempty"`

	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

type assistantMessage struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type permissionDenial struct {
	ToolName string `json:"tool_name"`
	Reason   string `json:"reason"`
}

type toolUseContent struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type toolResultContent struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

func handleStreamEvent(opts RunOpts, evt *streamEvent) {
	switch evt.Type {
	case "system":
		handleSystemEvent(opts, evt)
	case "assistant":
		if evt.Message != nil {
			handleAssistantMessage(opts, evt)
		} else {
			handleAssistantLegacy(opts, evt)
		}
	case "result":
		handleResultEvent(opts, evt)
	case "error":
		var errMsg string
		if err := json.Unmarshal(evt.Content, &errMsg); err != nil {
			errMsg = string(evt.Content)
		}
		opts.Emitter(protocol.TypeNotification, protocol.Notification{
			InstanceID: opts.InstanceID,
			Type:       "error",
			Message:    errMsg,
		})
	}
}

func handleSystemEvent(opts RunOpts, evt *streamEvent) {
	if evt.Model != "" || evt.PermissionMode != "" {
		opts.Emitter(protocol.TypeInstanceStatus, protocol.InstanceStatusMsg{
			InstanceID:     opts.InstanceID,
			Model:          evt.Model,
			PermissionMode: evt.PermissionMode,
		})
	}
}

func handleAssistantMessage(opts RunOpts, evt *streamEvent) {
	for _, block := range evt.Message.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				opts.Emitter(protocol.TypeStreamText, protocol.StreamText{
					InstanceID: opts.InstanceID,
					Text:       block.Text,
					IsPartial:  true,
				})
			}
		case "tool_use":
			opts.Emitter(protocol.TypeStreamToolUse, protocol.StreamToolUse{
				InstanceID: opts.InstanceID,
				ToolName:   block.Name,
				ToolInput:  block.Input,
				ToolUseID:  block.ID,
			})
		case "tool_result":
			opts.Emitter(protocol.TypeStreamToolResult, protocol.StreamToolResult{
				InstanceID: opts.InstanceID,
				ToolUseID:  block.ToolUseID,
				Output:     block.Content,
				IsError:    block.IsError,
			})
		}
	}
}

func handleAssistantLegacy(opts RunOpts, evt *streamEvent) {
	switch evt.Subtype {
	case "text":
		var text string
		if err := json.Unmarshal(evt.Content, &text); err != nil {
			log.Printf("[claude %s] invalid text content: %v", opts.InstanceID, err)
			return
		}
		opts.Emitter(protocol.TypeStreamText, protocol.StreamText{
			InstanceID: opts.InstanceID,
			Text:       text,
			IsPartial:  true,
		})
	case "tool_use":
		var tc toolUseContent
		if err := json.Unmarshal(evt.Content, &tc); err != nil {
			log.Printf("[claude %s] invalid tool_use content: %v", opts.InstanceID, err)
			return
		}
		opts.Emitter(protocol.TypeStreamToolUse, protocol.StreamToolUse{
			InstanceID: opts.InstanceID,
			ToolName:   tc.Name,
			ToolInput:  tc.Input,
			ToolUseID:  tc.ID,
		})
	case "tool_result":
		var tr toolResultContent
		if err := json.Unmarshal(evt.Content, &tr); err != nil {
			log.Printf("[claude %s] invalid tool_result content: %v", opts.InstanceID, err)
			return
		}
		opts.Emitter(protocol.TypeStreamToolResult, protocol.StreamToolResult{
			InstanceID: opts.InstanceID,
			ToolUseID:  tr.ToolUseID,
			Output:     tr.Content,
			IsError:    tr.IsError,
		})
	}
}

func handleResultEvent(opts RunOpts, evt *streamEvent) {
	opts.Emitter(protocol.TypeStreamText, protocol.StreamText{
		InstanceID: opts.InstanceID,
		Text:       evt.Result,
		IsPartial:  false,
	})
	opts.Emitter(protocol.TypeStreamComplete, protocol.StreamComplete{
		InstanceID: opts.InstanceID,
		CostUSD:    evt.CostUSD,
		DurationMS: int64(evt.Duration),
	})

	for _, denial := range evt.PermissionDenials {
		opts.Emitter(protocol.TypeNotification, protocol.Notification{
			InstanceID: opts.InstanceID,
			Type:       "permission_denied",
			Message:    fmt.Sprintf("Permission denied for %s: %s", denial.ToolName, denial.Reason),
		})
	}

	if evt.SessionID != "" && opts.OnSessionID != nil {
		opts.OnSessionID(evt.SessionID)
	}
}
