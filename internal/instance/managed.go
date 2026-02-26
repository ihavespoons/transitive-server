package instance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/ihavespoons/claudette-server/internal/protocol"
	"github.com/google/uuid"
)

// ManagedInstance is a Claude Code instance that spawns a new process per message.
// Session continuity is maintained via --resume with the session ID.
type ManagedInstance struct {
	id             string
	sessionID      string
	cwd            string
	model          string
	permissionMode string
	claudePath     string
	status         string
	sink           EventSink
	mu             sync.Mutex
	busy           bool // true while a prompt is being processed
	hasSession     bool // true after first successful prompt
}

// stream-json event types from Claude CLI.
type streamEvent struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`

	// For "result" type
	Result    string  `json:"result,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	Duration  float64 `json:"duration_ms,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	IsError   bool    `json:"is_error,omitempty"`
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

func NewManagedInstance(cwd, model, permissionMode, claudePath string, sink EventSink) *ManagedInstance {
	cwd = strings.TrimSpace(cwd)
	if strings.HasPrefix(cwd, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			cwd = home + cwd[1:]
		}
	}

	if err := os.MkdirAll(cwd, 0755); err != nil {
		log.Printf("failed to create working directory %s: %v", cwd, err)
	}

	return &ManagedInstance{
		id:             uuid.New().String(),
		sessionID:      uuid.New().String(),
		cwd:            cwd,
		model:          model,
		permissionMode: permissionMode,
		claudePath:     claudePath,
		status:         "running",
		sink:           sink,
	}
}

func (m *ManagedInstance) ID() string        { return m.id }
func (m *ManagedInstance) SessionID() string  { return m.sessionID }
func (m *ManagedInstance) Cwd() string        { return m.cwd }
func (m *ManagedInstance) Type() string       { return "managed" }

func (m *ManagedInstance) Status() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *ManagedInstance) Info() protocol.InstanceInfo {
	return protocol.InstanceInfo{
		InstanceID: m.id,
		SessionID:  m.sessionID,
		Status:     m.Status(),
		Cwd:        m.cwd,
		Type:       "managed",
	}
}

func (m *ManagedInstance) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = "stopped"
	return nil
}

// UpdateConfig updates the model and/or permission mode for subsequent prompts.
func (m *ManagedInstance) UpdateConfig(model, permissionMode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if model != "" {
		m.model = model
	}
	if permissionMode != "" {
		m.permissionMode = permissionMode
	}
}

// Resume marks a stopped instance as running again.
func (m *ManagedInstance) Resume() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = "running"
	return nil
}

// SendPrompt spawns a claude process for this message, streams output, then exits.
func (m *ManagedInstance) SendPrompt(text string) error {
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return fmt.Errorf("instance is busy processing another prompt")
	}
	m.busy = true
	m.mu.Unlock()

	go m.runPrompt(text)
	return nil
}

func (m *ManagedInstance) runPrompt(text string) {
	defer func() {
		m.mu.Lock()
		m.busy = false
		m.mu.Unlock()
	}()

	m.mu.Lock()
	hasSession := m.hasSession
	m.mu.Unlock()

	args := []string{
		"-p", text,
		"--output-format", "stream-json",
		"--verbose",
	}
	if hasSession {
		args = append(args, "--resume", m.sessionID)
	} else {
		args = append(args, "--session-id", m.sessionID)
	}
	if m.model != "" {
		args = append(args, "--model", m.model)
	}
	if m.permissionMode != "" {
		args = append(args, "--permission-mode", m.permissionMode)
	}

	cmd := exec.Command(m.claudePath, args...)
	cmd.Dir = m.cwd

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
		log.Printf("[managed %s] stdout pipe: %v", m.id, err)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("[managed %s] stderr pipe: %v", m.id, err)
		return
	}

	log.Printf("[managed %s] starting: claude -p %q --resume %s", m.id, text, m.sessionID)
	if err := cmd.Start(); err != nil {
		log.Printf("[managed %s] start failed: %v", m.id, err)
		m.emit(protocol.TypeNotification, protocol.Notification{
			InstanceID: m.id,
			Type:       "error",
			Message:    fmt.Sprintf("failed to start claude: %v", err),
		})
		return
	}

	// Read stderr in background.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[managed %s stderr] %s", m.id, scanner.Text())
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
		log.Printf("[managed %s stdout] %s", m.id, string(line))

		var evt streamEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			log.Printf("[managed %s] invalid JSON line: %v", m.id, err)
			continue
		}
		m.handleStreamEvent(&evt)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[managed %s] stream read error: %v", m.id, err)
	}

	// Wait for process to finish.
	if err := cmd.Wait(); err != nil {
		log.Printf("[managed %s] process exited: %v", m.id, err)
	} else {
		log.Printf("[managed %s] process exited cleanly", m.id)
	}
}

func (m *ManagedInstance) emit(msgType string, payload any) {
	if m.sink == nil {
		return
	}
	env, err := protocol.NewEnvelope(msgType, uuid.New().String(), payload)
	if err != nil {
		log.Printf("[managed %s] failed to create envelope: %v", m.id, err)
		return
	}
	m.sink(env)
}

func (m *ManagedInstance) handleStreamEvent(evt *streamEvent) {
	switch evt.Type {
	case "assistant":
		switch evt.Subtype {
		case "text":
			var text string
			if err := json.Unmarshal(evt.Content, &text); err != nil {
				log.Printf("[managed %s] invalid text content: %v", m.id, err)
				return
			}
			m.emit(protocol.TypeStreamText, protocol.StreamText{
				InstanceID: m.id,
				Text:       text,
				IsPartial:  true,
			})

		case "tool_use":
			var tc toolUseContent
			if err := json.Unmarshal(evt.Content, &tc); err != nil {
				log.Printf("[managed %s] invalid tool_use content: %v", m.id, err)
				return
			}
			m.emit(protocol.TypeStreamToolUse, protocol.StreamToolUse{
				InstanceID: m.id,
				ToolName:   tc.Name,
				ToolInput:  tc.Input,
				ToolUseID:  tc.ID,
			})

		case "tool_result":
			var tr toolResultContent
			if err := json.Unmarshal(evt.Content, &tr); err != nil {
				log.Printf("[managed %s] invalid tool_result content: %v", m.id, err)
				return
			}
			m.emit(protocol.TypeStreamToolResult, protocol.StreamToolResult{
				InstanceID: m.id,
				ToolUseID:  tr.ToolUseID,
				Output:     tr.Content,
				IsError:    tr.IsError,
			})
		}

	case "result":
		m.emit(protocol.TypeStreamText, protocol.StreamText{
			InstanceID: m.id,
			Text:       evt.Result,
			IsPartial:  false,
		})
		m.emit(protocol.TypeStreamComplete, protocol.StreamComplete{
			InstanceID: m.id,
			CostUSD:    evt.CostUSD,
			DurationMS: int64(evt.Duration),
		})

		if evt.SessionID != "" {
			m.mu.Lock()
			m.sessionID = evt.SessionID
			m.hasSession = true
			m.mu.Unlock()
		}

	case "error":
		var errMsg string
		if err := json.Unmarshal(evt.Content, &errMsg); err != nil {
			errMsg = string(evt.Content)
		}
		m.emit(protocol.TypeNotification, protocol.Notification{
			InstanceID: m.id,
			Type:       "error",
			Message:    errMsg,
		})
	}
}
