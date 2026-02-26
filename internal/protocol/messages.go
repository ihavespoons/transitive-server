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
	InstanceID string `json:"instance_id"`
	SessionID  string `json:"session_id"`
	Cwd        string `json:"cwd"`
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

// --- Mobile → Server payloads ---

type PromptSend struct {
	InstanceID string `json:"instance_id"`
	Text       string `json:"text"`
}

type InstanceLaunchRequest struct {
	Cwd            string `json:"cwd"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

type InstanceStopRequest struct {
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

	// Mobile → Server
	TypePromptSend            = "prompt.send"
	TypeInstanceLaunch        = "instance.launch"
	TypeInstanceStop          = "instance.stop"
	TypeInstanceListRequest   = "instance.list.request"
	TypePermissionRespond     = "permission.respond"
	TypeInstanceConfigUpdate  = "instance.config.update"
)
