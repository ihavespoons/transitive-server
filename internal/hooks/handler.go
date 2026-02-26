package hooks

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ihavespoons/claudette-server/internal/instance"
	"github.com/ihavespoons/claudette-server/internal/protocol"
	"github.com/google/uuid"
)

const permissionTimeout = 5 * time.Minute

// HookEvent is the JSON payload POSTed by the hook script.
type HookEvent struct {
	// Common fields from Claude Code hooks
	SessionID string          `json:"session_id"`
	Event     string          `json:"event"` // PreToolUse, PostToolUse, etc.
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Cwd       string          `json:"cwd,omitempty"`

	// These come from the hook's stdin as the full JSON blob
	// We also accept the raw data directly
}

// pendingPermission tracks a blocking permission request.
type pendingPermission struct {
	ch chan string // receives "allow" or "deny"
}

// Handler is the HTTP handler for hook POSTs.
type Handler struct {
	manager *instance.Manager
	sink    instance.EventSink

	permMu      sync.Mutex
	permissions map[string]*pendingPermission
}

func NewHandler(mgr *instance.Manager, sink instance.EventSink) *Handler {
	return &Handler{
		manager:     mgr,
		sink:        sink,
		permissions: make(map[string]*pendingPermission),
	}
}

func (h *Handler) emit(msgType string, payload any) {
	if h.sink == nil {
		return
	}
	env, err := protocol.NewEnvelope(msgType, uuid.New().String(), payload)
	if err != nil {
		log.Printf("[hooks] failed to create envelope: %v", err)
		return
	}
	h.sink(env)
}

// ServeHTTP handles POST /hook from the shell script.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	// Parse the hook event - Claude Code sends different structures per hook type.
	// We try to parse common fields.
	var evt HookEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		log.Printf("[hooks] invalid JSON: %v", err)
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// Try to get or create an attached instance for this session.
	instanceID := h.ensureAttachedInstance(evt.SessionID, evt.Cwd)

	switch evt.Event {
	case "PreToolUse":
		h.handlePreToolUse(w, instanceID, evt, body)
		return

	case "PostToolUse":
		h.handlePostToolUse(instanceID, evt)

	case "Notification":
		h.handleNotification(instanceID, body)

	case "Stop":
		h.handleStop(instanceID)

	default:
		log.Printf("[hooks] unhandled event: %s", evt.Event)
	}

	w.WriteHeader(http.StatusOK)
}

func (h *Handler) ensureAttachedInstance(sessionID, cwd string) string {
	if sessionID == "" {
		return ""
	}

	// Check if we already track this session.
	for _, info := range h.manager.List() {
		if info.SessionID == sessionID {
			return info.InstanceID
		}
	}

	// Create a new attached instance.
	inst := instance.NewAttachedInstance(uuid.New().String(), sessionID, cwd, "")
	h.manager.Add(inst)

	h.emit(protocol.TypeInstanceDiscovered, protocol.InstanceDiscovered{
		InstanceID: inst.ID(),
		SessionID:  sessionID,
		Cwd:        cwd,
	})

	return inst.ID()
}

// blockedPath checks if the tool input references a protected file.
func blockedPath(toolInput json.RawMessage) bool {
	var input map[string]any
	if json.Unmarshal(toolInput, &input) != nil {
		return false
	}

	// Check common path fields used by Claude Code tools.
	for _, key := range []string{"file_path", "path", "command"} {
		val, ok := input[key].(string)
		if !ok {
			continue
		}
		if containsCredentialsPath(val) {
			return true
		}
	}
	return false
}

func containsCredentialsPath(s string) bool {
	credDir := ".claudette"
	if u, err := user.Current(); err == nil {
		credDir = filepath.Join(u.HomeDir, ".claudette")
	}
	return strings.Contains(s, credDir)
}

func (h *Handler) handlePreToolUse(w http.ResponseWriter, instanceID string, evt HookEvent, rawBody []byte) {
	// Block access to claudette credentials.
	if blockedPath(evt.ToolInput) {
		log.Printf("[hooks] blocked access to protected path: tool=%s instance=%s", evt.ToolName, instanceID)
		resp := map[string]any{"decision": "deny", "reason": "access to claudette credentials is not allowed"}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	// Emit tool use event.
	toolUseID := uuid.New().String()
	h.emit(protocol.TypeStreamToolUse, protocol.StreamToolUse{
		InstanceID: instanceID,
		ToolName:   evt.ToolName,
		ToolInput:  evt.ToolInput,
		ToolUseID:  toolUseID,
	})

	// Also emit a permission request and block until the mobile user responds.
	requestID := uuid.New().String()
	h.emit(protocol.TypePermissionRequest, protocol.PermissionRequestMsg{
		InstanceID: instanceID,
		RequestID:  requestID,
		ToolName:   evt.ToolName,
		ToolInput:  evt.ToolInput,
	})

	// Create a pending permission and block.
	perm := &pendingPermission{ch: make(chan string, 1)}
	h.permMu.Lock()
	h.permissions[requestID] = perm
	h.permMu.Unlock()

	defer func() {
		h.permMu.Lock()
		delete(h.permissions, requestID)
		h.permMu.Unlock()
	}()

	// Wait for response or timeout.
	select {
	case decision := <-perm.ch:
		// Return decision to Claude Code via hook response.
		resp := map[string]any{
			"decision": decision,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)

	case <-time.After(permissionTimeout):
		log.Printf("[hooks] permission request %s timed out", requestID)
		w.WriteHeader(http.StatusOK) // Return empty = no decision.
	}
}

func (h *Handler) handlePostToolUse(instanceID string, evt HookEvent) {
	// We could emit tool result here if we had the output.
	// For now just log it.
	log.Printf("[hooks] PostToolUse: instance=%s tool=%s", instanceID, evt.ToolName)
}

func (h *Handler) handleNotification(instanceID string, body []byte) {
	// Forward notification to mobile.
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return
	}
	msg := ""
	if m, ok := raw["message"].(string); ok {
		msg = m
	}
	h.emit(protocol.TypeNotification, protocol.Notification{
		InstanceID: instanceID,
		Type:       "notification",
		Message:    msg,
	})
}

func (h *Handler) handleStop(instanceID string) {
	inst := h.manager.Get(instanceID)
	if inst != nil {
		if attached, ok := inst.(*instance.AttachedInstance); ok {
			attached.SetStatus("stopped")
		}
	}
	h.emit(protocol.TypeInstanceStopped, protocol.InstanceStopped{
		InstanceID: instanceID,
		Reason:     "session ended",
	})
}

// ResolvePermission is called when the mobile user responds to a permission request.
func (h *Handler) ResolvePermission(requestID, decision string) {
	h.permMu.Lock()
	perm, ok := h.permissions[requestID]
	h.permMu.Unlock()
	if ok {
		perm.ch <- decision
	}
}
