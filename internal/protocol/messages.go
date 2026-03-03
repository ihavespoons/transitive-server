package protocol

import (
	"encoding/json"
	"time"
)

// Envelope is the wire format for all messages over WebSocket.
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	ReplyTo string          `json:"reply_to,omitempty"`
	TS      int64           `json:"ts"`
	Payload json.RawMessage `json:"payload"`
}

// NewEnvelope creates an Envelope with the given type and payload.
func NewEnvelope(msgType string, id string, payload any) (*Envelope, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &Envelope{
		Type:    msgType,
		ID:      id,
		TS:      time.Now().UnixMilli(),
		Payload: data,
	}, nil
}

// --- Server → Mobile payloads ---

type InstanceDiscovered struct {
	InstanceID  string `json:"instance_id"`
	SessionID   string `json:"session_id"`
	Cwd         string `json:"cwd"`
	ProjectName string `json:"project_name"`
}

type InstanceLaunched struct {
	InstanceID     string `json:"instance_id"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	BackendID      string `json:"backend_id,omitempty"`
}

type InstanceStopped struct {
	InstanceID string `json:"instance_id"`
	Reason     string `json:"reason"`
}

type InstanceInfo struct {
	InstanceID string `json:"instance_id"`
	SessionID  string `json:"session_id"`
	Status     string `json:"status"` // "running", "stopped"
	Cwd        string `json:"cwd"`
	Type       string `json:"type"` // "managed", "attached"
	BackendID  string `json:"backend_id,omitempty"`
}

type InstanceList struct {
	Instances []InstanceInfo `json:"instances"`
}

type StreamText struct {
	InstanceID string `json:"instance_id"`
	Text       string `json:"text"`
	IsPartial  bool   `json:"is_partial"`
}

type StreamToolUse struct {
	InstanceID string          `json:"instance_id"`
	ToolName   string          `json:"tool_name"`
	ToolInput  json.RawMessage `json:"tool_input"`
	ToolUseID  string          `json:"tool_use_id"`
}

type StreamToolResult struct {
	InstanceID string `json:"instance_id"`
	ToolUseID  string `json:"tool_use_id"`
	Output     string `json:"output"`
	IsError    bool   `json:"is_error"`
}

type StreamComplete struct {
	InstanceID string  `json:"instance_id"`
	CostUSD    float64 `json:"cost_usd"`
	DurationMS int64   `json:"duration_ms"`
}

type PermissionRequestMsg struct {
	InstanceID string          `json:"instance_id"`
	RequestID  string          `json:"request_id"`
	ToolName   string          `json:"tool_name"`
	ToolInput  json.RawMessage `json:"tool_input"`
}

type Notification struct {
	InstanceID string `json:"instance_id"`
	Type       string `json:"type"`
	Message    string `json:"message"`
}

type PlanReview struct {
	InstanceID string `json:"instance_id"`
	RequestID  string `json:"request_id"`
	Plan       string `json:"plan"`
}

type AskUserQuestion struct {
	InstanceID string          `json:"instance_id"`
	RequestID  string          `json:"request_id"`
	Questions  json.RawMessage `json:"questions"`
}

type InstanceStatusMsg struct {
	InstanceID     string `json:"instance_id"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

type ServerConfig struct {
	ProjectDir string `json:"project_dir"`
}

// Backend discovery payloads.

type BackendListMsg struct {
	Backends []BackendInfoMsg `json:"backends"`
}

type BackendInfoMsg struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Available bool              `json:"available"`
	Providers []ProviderInfoMsg `json:"providers"`
}

type ProviderInfoMsg struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

// --- Mobile → Server payloads ---

type PromptSend struct {
	InstanceID string `json:"instance_id"`
	Text       string `json:"text"`
}

type InstanceLaunchRequest struct {
	Cwd            string `json:"cwd"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	RepositoryURL  string `json:"repository_url,omitempty"`
	BackendID      string `json:"backend_id,omitempty"`
}

type InstanceStopRequest struct {
	InstanceID string `json:"instance_id"`
}

type InstanceRemoveRequest struct {
	InstanceID string `json:"instance_id"`
}

type InstanceListRequest struct{}

type PermissionResponse struct {
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"` // "allow" or "deny"
}

type InstanceConfigUpdate struct {
	InstanceID     string `json:"instance_id"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

type InstanceAdoptRequest struct {
	InstanceID string `json:"instance_id"`
}

// --- Shell payloads ---

type ShellStart struct {
	InstanceID string `json:"instance_id"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
}

type ShellInput struct {
	InstanceID string `json:"instance_id"`
	Data       string `json:"data"` // base64
}

type ShellResize struct {
	InstanceID string `json:"instance_id"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
}

type ShellStopRequest struct {
	InstanceID string `json:"instance_id"`
}

type ShellStarted struct {
	InstanceID string `json:"instance_id"`
}

type ShellOutput struct {
	InstanceID string `json:"instance_id"`
	Data       string `json:"data"` // base64
}

type ShellStopped struct {
	InstanceID string `json:"instance_id"`
	Reason     string `json:"reason"`
}

// --- Background process payloads ---

type BackgroundProcessStarted struct {
	InstanceID string `json:"instance_id"`
	ProcessID  string `json:"process_id"`
	Name       string `json:"name"`
	Command    string `json:"command"`
	Pid        int    `json:"pid"`
	Port       int    `json:"port,omitempty"`
}

type BackgroundProcessStopped struct {
	InstanceID string `json:"instance_id"`
	ProcessID  string `json:"process_id"`
	Reason     string `json:"reason"`
}

type BackgroundProcessOutput struct {
	InstanceID string `json:"instance_id"`
	ProcessID  string `json:"process_id"`
	Data       string `json:"data"` // base64
}

type BackgroundProcessInfo struct {
	ProcessID string `json:"process_id"`
	Name      string `json:"name"`
	Command   string `json:"command"`
	Port      int    `json:"port,omitempty"`
	Pid       int    `json:"pid"`
	Status    string `json:"status"` // "running", "stopped", "errored"
	StartedAt int64  `json:"started_at"`
}

type BackgroundProcessList struct {
	InstanceID string                  `json:"instance_id"`
	Processes  []BackgroundProcessInfo `json:"processes"`
}

type BackgroundProcessError struct {
	InstanceID string `json:"instance_id"`
	ProcessID  string `json:"process_id,omitempty"`
	Error      string `json:"error"`
}

// Mobile → Server background process payloads.

type BackgroundProcessStartRequest struct {
	InstanceID string `json:"instance_id"`
	Command    string `json:"command"`
	Name       string `json:"name"`
	Port       int    `json:"port,omitempty"`
}

type BackgroundProcessStopRequest struct {
	InstanceID string `json:"instance_id"`
	ProcessID  string `json:"process_id"`
}

type BackgroundProcessRestartRequest struct {
	InstanceID string `json:"instance_id"`
	ProcessID  string `json:"process_id"`
}

type BackgroundProcessListRequest struct {
	InstanceID string `json:"instance_id"`
}

// --- Port forward payloads ---

type PortForwardStart struct {
	InstanceID string `json:"instance_id"`
	Port       int    `json:"port"`
}

type PortForwardStopRequest struct {
	InstanceID string `json:"instance_id"`
	Port       int    `json:"port"`
}

type PortForwardStarted struct {
	InstanceID string `json:"instance_id"`
	Port       int    `json:"port"`
	ProxyURL   string `json:"proxy_url"`
}

type PortForwardStopped struct {
	InstanceID string `json:"instance_id"`
	Port       int    `json:"port"`
}

type PortForwardError struct {
	InstanceID string `json:"instance_id"`
	Port       int    `json:"port"`
	Error      string `json:"error"`
}

type PortForwardListRequest struct {
	InstanceID string `json:"instance_id"`
}

type PortForwardList struct {
	InstanceID string            `json:"instance_id"`
	Forwards   []PortForwardInfo `json:"forwards"`
}

type PortForwardInfo struct {
	Port     int    `json:"port"`
	ProxyURL string `json:"proxy_url"`
}

// --- Proxy payloads (relay ↔ server) ---

type ProxyRegister struct {
	Token string `json:"token"`
	Port  int    `json:"port"`
}

type ProxyUnregister struct {
	Token string `json:"token"`
}

type ProxyRequest struct {
	ID      string            `json:"id"`
	Port    int               `json:"port"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64
}

type ProxyResponse struct {
	ID         string            `json:"id"`
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	BodyBase64 string            `json:"body_base64"`
	Seq        int               `json:"seq"`
	Done       bool              `json:"done"`
}

// Message type constants.
const (
	// Server → Mobile
	TypeInstanceDiscovered = "instance.discovered"
	TypeInstanceLaunched   = "instance.launched"
	TypeInstanceStopped    = "instance.stopped"
	TypeInstanceList       = "instance.list"
	TypeStreamText         = "stream.text"
	TypeStreamToolUse      = "stream.tool_use"
	TypeStreamToolResult   = "stream.tool_result"
	TypeStreamComplete     = "stream.complete"
	TypePermissionRequest  = "permission.request"
	TypeNotification       = "notification"
	TypePlanReview         = "plan.review"
	TypeAskUserQuestion    = "ask.user.question"
	TypeInstanceStatus     = "instance.status"
	TypeServerConfig       = "server.config"
	TypeBackendList        = "backend.list"

	// Mobile → Server
	TypePromptSend            = "prompt.send"
	TypeInstanceLaunch        = "instance.launch"
	TypeInstanceStop          = "instance.stop"
	TypeInstanceRemove        = "instance.remove"
	TypeInstanceListRequest   = "instance.list.request"
	TypePermissionRespond     = "permission.respond"
	TypeAskUserAnswer         = "ask.user.answer"
	TypeInstanceConfigUpdate  = "instance.config.update"
	TypeInstanceAdopt         = "instance.adopt"
	TypeBackendListRequest    = "backend.list.request"

	// Shell (mobile → server)
	TypeShellStart  = "shell.start"
	TypeShellInput  = "shell.input"
	TypeShellResize = "shell.resize"
	TypeShellStop   = "shell.stop"

	// Shell (server → mobile)
	TypeShellStarted = "shell.started"
	TypeShellOutput  = "shell.output"
	TypeShellStopped = "shell.stopped"

	// Background process (mobile → server)
	TypeBgProcessStart       = "background.process.start"
	TypeBgProcessStop        = "background.process.stop"
	TypeBgProcessRestart     = "background.process.restart"
	TypeBgProcessListRequest = "background.process.list.request"

	// Background process (server → mobile)
	TypeBgProcessStarted = "background.process.started"
	TypeBgProcessStopped = "background.process.stopped"
	TypeBgProcessOutput  = "background.process.output"
	TypeBgProcessList    = "background.process.list"
	TypeBgProcessError   = "background.process.error"

	// Port forward (mobile → server)
	TypePortForwardStart       = "port_forward.start"
	TypePortForwardStop        = "port_forward.stop"
	TypePortForwardListRequest = "port_forward.list.request"

	// Port forward (server → mobile)
	TypePortForwardStarted = "port_forward.started"
	TypePortForwardStopped = "port_forward.stopped"
	TypePortForwardError   = "port_forward.error"
	TypePortForwardList    = "port_forward.list"

	// Proxy internal (relay ↔ server)
	TypeProxyRegister   = "proxy.register"
	TypeProxyUnregister = "proxy.unregister"
	TypeProxyRequest    = "proxy.request"
	TypeProxyResponse   = "proxy.response"
)
