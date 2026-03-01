package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ihavespoons/transitive/internal/bgprocess"
	"github.com/ihavespoons/transitive/internal/instance"
	"github.com/ihavespoons/transitive/internal/protocol"
	"github.com/google/uuid"
)

const permissionTimeout = 5 * time.Minute

// HookEvent is the JSON payload POSTed by the hook script.
type HookEvent struct {
	SessionID      string          `json:"session_id"`
	Event          string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   json.RawMessage `json:"tool_response,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
	Cwd            string          `json:"cwd,omitempty"`
	TranscriptPath string          `json:"transcript_path,omitempty"`
	// Stop event fields
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
	// Notification event fields
	Message          string `json:"message,omitempty"`
	NotificationType string `json:"notification_type,omitempty"`
}

// pendingPermission tracks a blocking permission request.
type pendingPermission struct {
	ch chan map[string]any // carries response data (at minimum "decision")
}

// Handler is the HTTP handler for hook POSTs.
type Handler struct {
	manager    *instance.Manager
	sink       instance.EventSink
	bgprocess  *bgprocess.Manager

	permMu      sync.Mutex
	permissions map[string]*pendingPermission
}

func NewHandler(mgr *instance.Manager, sink instance.EventSink, bgMgr *bgprocess.Manager) *Handler {
	return &Handler{
		manager:     mgr,
		sink:        sink,
		bgprocess:   bgMgr,
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

	// Extract CLI PID from hook subprocess.
	var claudePID int
	if pidStr := r.Header.Get("X-Claude-PID"); pidStr != "" {
		fmt.Sscanf(pidStr, "%d", &claudePID)
	}

	// Backend identifier (empty defaults to "claude" for backward compat).
	backendID := r.Header.Get("X-Backend")

	// If X-Instance-ID is set and refers to a known managed instance, route
	// directly to it instead of creating a ghost attached instance.
	instanceID := r.Header.Get("X-Instance-ID")
	if instanceID != "" {
		inst := h.manager.Get(instanceID)
		if inst == nil || inst.Type() != "managed" {
			instanceID = "" // fall through to ensureAttachedInstance
		}
	}
	if instanceID == "" {
		instanceID = h.ensureAttachedInstance(evt.SessionID, evt.Cwd, claudePID, backendID)
	}

	switch evt.Event {
	case "PreToolUse":
		h.handlePreToolUse(w, instanceID, evt, body)
		return

	case "PostToolUse":
		// For managed instances the NDJSON stream already emits tool results;
		// only forward PostToolUse for attached (terminal) instances.
		if !h.isManaged(instanceID) {
			h.handlePostToolUse(instanceID, evt)
		}

	case "Notification":
		h.handleNotification(instanceID, body)

	case "Stop":
		// For managed instances the NDJSON stream handles completion;
		// only process Stop for attached (terminal) instances.
		if !h.isManaged(instanceID) {
			h.handleStop(instanceID, evt)
		}

	case "SessionEnd":
		if !h.isManaged(instanceID) {
			h.handleSessionEnd(instanceID)
		}

	case "SessionStart", "UserPromptSubmit", "SubagentStart", "SubagentStop",
		"PostToolUseFailure", "PermissionRequest", "PreCompact",
		"ConfigChange", "TeammateIdle", "TaskCompleted":
		// Known events that don't require server-side handling.

	default:
		log.Printf("[hooks] unhandled event: %s", evt.Event)
	}

	// Return schema-compliant JSON for the hook event type.
	w.Header().Set("Content-Type", "application/json")
	switch evt.Event {
	case "PostToolUse":
		w.Write([]byte(`{"hookEventName":"PostToolUse"}`))
	case "Stop":
		w.Write([]byte(`{"stopReason":"completed"}`))
	case "UserPromptSubmit":
		w.Write([]byte(`{"hookEventName":"UserPromptSubmit"}`))
	default:
		w.Write([]byte(`{}`))
	}
}

// ensureAttachedInstance gets or creates an attached instance for this session.
func (h *Handler) ensureAttachedInstance(sessionID, cwd string, pid int, backendID string) string {
	if sessionID == "" {
		return ""
	}

	// Check if we already track this session.
	for _, info := range h.manager.List() {
		if info.SessionID == sessionID {
			// Update PID on existing attached instance.
			if pid > 0 {
				if inst := h.manager.Get(info.InstanceID); inst != nil {
					if attached, ok := inst.(*instance.AttachedInstance); ok {
						attached.SetPID(pid)
					}
				}
			}
			return info.InstanceID
		}
	}

	// Create a new attached instance.
	projectName := filepath.Base(cwd)
	inst := instance.NewAttachedInstance(uuid.New().String(), sessionID, cwd, projectName)
	if pid > 0 {
		inst.SetPID(pid)
	}
	if backendID != "" {
		inst.SetBackendID(backendID)
	}
	h.manager.Add(inst)

	h.emit(protocol.TypeInstanceDiscovered, protocol.InstanceDiscovered{
		InstanceID:  inst.ID(),
		SessionID:   sessionID,
		Cwd:         cwd,
		ProjectName: projectName,
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
	credDir := ".transitive"
	if u, err := user.Current(); err == nil {
		credDir = filepath.Join(u.HomeDir, ".transitive")
	}
	return strings.Contains(s, credDir)
}

// readOnlyTools are auto-approved without prompting mobile.
// Includes both Claude Code and OpenCode tool names.
var readOnlyTools = map[string]bool{
	"Read":       true,
	"Glob":       true,
	"Grep":       true,
	"WebFetch":   true,
	"WebSearch":  true,
	"TaskList":   true,
	"TaskGet":    true,
	// OpenCode tool name variants.
	"read":      true,
	"glob":      true,
	"grep":      true,
	"webfetch":  true,
	"websearch": true,
	"list_files": true,
	"search":     true,
}

func (h *Handler) isManaged(instanceID string) bool {
	inst := h.manager.Get(instanceID)
	return inst != nil && inst.Type() == "managed"
}

func (h *Handler) isBypass(instanceID string) bool {
	inst := h.manager.Get(instanceID)
	if inst == nil {
		return false
	}
	switch v := inst.(type) {
	case *instance.ManagedInstance:
		return v.PermissionMode() == "bypass"
	case *instance.AttachedInstance:
		return v.PermissionMode() == "bypass"
	}
	return false
}

// writePreToolAllow writes a schema-compliant PreToolUse allow response.
func writePreToolAllow(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"hookEventName":     "PreToolUse",
		"permissionDecision": "allow",
	})
}

// writePreToolDeny writes a schema-compliant PreToolUse deny response.
func writePreToolDeny(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"hookEventName":             "PreToolUse",
		"permissionDecision":        "deny",
		"permissionDecisionReason":  reason,
	})
}

func (h *Handler) handlePreToolUse(w http.ResponseWriter, instanceID string, evt HookEvent, rawBody []byte) {
	// Block access to transitive credentials.
	if blockedPath(evt.ToolInput) {
		log.Printf("[hooks] blocked access to protected path: tool=%s instance=%s", evt.ToolName, instanceID)
		writePreToolDeny(w, "access to transitive credentials is not allowed")
		return
	}

	// background_process tool: auto-allow and handle directly.
	if evt.ToolName == "background_process" {
		h.handleBackgroundProcess(w, instanceID, evt)
		return
	}

	managed := h.isManaged(instanceID)

	// For attached instances, emit tool_use (managed instances already emit via stream).
	if !managed {
		toolUseID := uuid.New().String()
		h.emit(protocol.TypeStreamToolUse, protocol.StreamToolUse{
			InstanceID: instanceID,
			ToolName:   evt.ToolName,
			ToolInput:  evt.ToolInput,
			ToolUseID:  toolUseID,
		})
	}

	// Non-managed (attached) instances are auto-approved for everything.
	// Only managed instances get interactive prompts and permission gates.
	if !managed {
		writePreToolAllow(w)
		return
	}

	// Interactive tools always route to mobile — they require user input
	// regardless of bypass mode (questions need answers, plans need review).
	// Match both Claude Code and OpenCode tool name variants.
	if evt.ToolName == "ExitPlanMode" {
		h.handleExitPlanMode(w, instanceID, evt)
		return
	}
	if evt.ToolName == "AskUserQuestion" || evt.ToolName == "question" {
		h.handleAskUserQuestion(w, instanceID, evt)
		return
	}

	// Bypass mode: auto-approve all other tools for managed instances.
	if h.isBypass(instanceID) {
		writePreToolAllow(w)
		return
	}

	// Auto-approve read-only tools for managed instances.
	if readOnlyTools[evt.ToolName] {
		writePreToolAllow(w)
		return
	}

	// Write tools / other tools: route to mobile for permission.
	requestID := uuid.New().String()
	h.emit(protocol.TypePermissionRequest, protocol.PermissionRequestMsg{
		InstanceID: instanceID,
		RequestID:  requestID,
		ToolName:   evt.ToolName,
		ToolInput:  evt.ToolInput,
	})

	h.blockOnPermission(w, requestID, instanceID, evt.ToolName)
}

func (h *Handler) handleExitPlanMode(w http.ResponseWriter, instanceID string, evt HookEvent) {
	// Extract plan text from tool input.
	var input map[string]any
	plan := ""
	if err := json.Unmarshal(evt.ToolInput, &input); err == nil {
		// ExitPlanMode input may have allowedPrompts or other fields;
		// the plan itself is read from the plan file by Claude Code.
		// We send the raw input as context.
		if p, ok := input["plan"].(string); ok {
			plan = p
		}
	}
	if plan == "" {
		// Fallback: stringify the entire input.
		plan = string(evt.ToolInput)
	}

	requestID := uuid.New().String()
	h.emit(protocol.TypePlanReview, protocol.PlanReview{
		InstanceID: instanceID,
		RequestID:  requestID,
		Plan:       plan,
	})

	h.blockOnPermission(w, requestID, instanceID, "ExitPlanMode")
}

func (h *Handler) handleAskUserQuestion(w http.ResponseWriter, instanceID string, evt HookEvent) {
	// Extract questions array from tool input.
	var input map[string]json.RawMessage
	if err := json.Unmarshal(evt.ToolInput, &input); err != nil {
		log.Printf("[hooks] failed to parse AskUserQuestion input: %v", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	questions, ok := input["questions"]
	if !ok {
		questions = evt.ToolInput
	}

	requestID := uuid.New().String()
	h.emit(protocol.TypeAskUserQuestion, protocol.AskUserQuestion{
		InstanceID: instanceID,
		RequestID:  requestID,
		Questions:  questions,
	})

	h.blockOnPermission(w, requestID, instanceID, "AskUserQuestion")
}

func (h *Handler) blockOnPermission(w http.ResponseWriter, requestID, instanceID, toolName string) {
	perm := &pendingPermission{ch: make(chan map[string]any, 1)}
	h.permMu.Lock()
	h.permissions[requestID] = perm
	h.permMu.Unlock()

	defer func() {
		h.permMu.Lock()
		delete(h.permissions, requestID)
		h.permMu.Unlock()
	}()

	select {
	case resp := <-perm.ch:
		// Translate mobile response to Claude Code's expected schema.
		decision, _ := resp["decision"].(string)
		permDecision := "allow"
		if decision == "deny" {
			permDecision = "deny"
		}
		hookResp := map[string]any{
			"hookEventName":     "PreToolUse",
			"permissionDecision": permDecision,
		}
		// For AskUserQuestion, pass through the answers.
		if answers, ok := resp["answers"]; ok {
			hookResp["answers"] = answers
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(hookResp)
	case <-time.After(permissionTimeout):
		log.Printf("[hooks] permission request %s timed out for %s on instance %s", requestID, toolName, instanceID)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"timed out"}`))
	}
}

func (h *Handler) handleBackgroundProcess(w http.ResponseWriter, instanceID string, evt HookEvent) {
	var input struct {
		Action    string `json:"action"`
		Command   string `json:"command"`
		Name      string `json:"name"`
		ProcessID string `json:"process_id"`
		Port      int    `json:"port"`
	}
	if err := json.Unmarshal(evt.ToolInput, &input); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"hookEventName":     "PreToolUse",
			"permissionDecision": "allow",
			"toolResult":        map[string]any{"error": "invalid input: " + err.Error()},
		})
		return
	}

	var result any
	var resultErr error

	switch input.Action {
	case "start":
		if h.bgprocess == nil {
			result = map[string]any{"error": "background process manager not available"}
			break
		}
		if input.Command == "" {
			result = map[string]any{"error": "command is required"}
			break
		}
		name := input.Name
		if name == "" {
			name = input.Command
		}
		processID, err := h.bgprocess.Start(instanceID, name, input.Command, input.Port)
		if err != nil {
			result = map[string]any{"error": err.Error()}
		} else {
			result = map[string]any{"process_id": processID, "status": "started", "name": name}
		}

	case "stop":
		if h.bgprocess == nil {
			result = map[string]any{"error": "background process manager not available"}
			break
		}
		if input.ProcessID == "" {
			result = map[string]any{"error": "process_id is required"}
			break
		}
		resultErr = h.bgprocess.Stop(input.ProcessID)
		if resultErr != nil {
			result = map[string]any{"error": resultErr.Error()}
		} else {
			result = map[string]any{"status": "stopped", "process_id": input.ProcessID}
		}

	case "restart":
		if h.bgprocess == nil {
			result = map[string]any{"error": "background process manager not available"}
			break
		}
		if input.ProcessID == "" {
			result = map[string]any{"error": "process_id is required"}
			break
		}
		newID, err := h.bgprocess.Restart(input.ProcessID)
		if err != nil {
			result = map[string]any{"error": err.Error()}
		} else {
			result = map[string]any{"status": "restarted", "process_id": newID}
		}

	case "list":
		if h.bgprocess == nil {
			result = map[string]any{"processes": []any{}}
			break
		}
		procs := h.bgprocess.List(instanceID)
		result = map[string]any{"processes": procs}

	default:
		result = map[string]any{"error": fmt.Sprintf("unknown action: %s", input.Action)}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"hookEventName":     "PreToolUse",
		"permissionDecision": "allow",
		"toolResult":        result,
	})
}

func (h *Handler) handlePostToolUse(instanceID string, evt HookEvent) {
	output := string(evt.ToolResponse)
	if output == "" {
		output = "(no output)"
	}
	h.emit(protocol.TypeStreamToolResult, protocol.StreamToolResult{
		InstanceID: instanceID,
		ToolUseID:  evt.ToolUseID,
		Output:     output,
		IsError:    false,
	})
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

func (h *Handler) handleStop(instanceID string, evt HookEvent) {
	// Emit the final assistant message if present.
	if evt.LastAssistantMessage != "" {
		h.emit(protocol.TypeStreamText, protocol.StreamText{
			InstanceID: instanceID,
			Text:       evt.LastAssistantMessage,
			IsPartial:  false,
		})
	}

	// Stop = end of turn. For attached instances the CLI is still running,
	// so signal turn_complete rather than marking as stopped.
	inst := h.manager.Get(instanceID)
	if inst != nil {
		if _, ok := inst.(*instance.AttachedInstance); ok {
			h.emit(protocol.TypeInstanceStopped, protocol.InstanceStopped{
				InstanceID: instanceID,
				Reason:     "turn_complete",
			})
			return
		}
	}

	h.emit(protocol.TypeInstanceStopped, protocol.InstanceStopped{
		InstanceID: instanceID,
		Reason:     "session ended",
	})
}

func (h *Handler) handleSessionEnd(instanceID string) {
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
func (h *Handler) ResolvePermission(requestID string, response map[string]any) {
	h.permMu.Lock()
	perm, ok := h.permissions[requestID]
	h.permMu.Unlock()
	if ok {
		perm.ch <- response
	}
}
