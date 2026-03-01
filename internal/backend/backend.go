package backend

import (
	"context"
	"encoding/json"
	"sync"
)

// ProviderInfo describes an AI provider available through a backend.
type ProviderInfo struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

// BackendInfo describes a backend and its available providers.
type BackendInfo struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Available bool           `json:"available"`
	Providers []ProviderInfo `json:"providers"`
}

// StreamEmitter sends a protocol event to the mobile client.
type StreamEmitter func(msgType string, payload any)

// PermissionResolver handles interactive permission and question responses
// from the mobile client back to the backend.
type PermissionResolver interface {
	WaitForResponse(ctx context.Context, requestID string) (map[string]any, error)
	Resolve(requestID string, response map[string]any)
}

// ChanPermissionResolver is a channel-based PermissionResolver.
type ChanPermissionResolver struct {
	mu       sync.Mutex
	pending  map[string]chan map[string]any
}

func NewChanPermissionResolver() *ChanPermissionResolver {
	return &ChanPermissionResolver{
		pending: make(map[string]chan map[string]any),
	}
}

func (r *ChanPermissionResolver) WaitForResponse(ctx context.Context, requestID string) (map[string]any, error) {
	ch := make(chan map[string]any, 1)
	r.mu.Lock()
	r.pending[requestID] = ch
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.pending, requestID)
		r.mu.Unlock()
	}()

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *ChanPermissionResolver) Resolve(requestID string, response map[string]any) {
	r.mu.Lock()
	ch, ok := r.pending[requestID]
	r.mu.Unlock()
	if ok {
		select {
		case ch <- response:
		default:
		}
	}
}

// RunOpts holds options for running a prompt through a backend.
type RunOpts struct {
	Ctx                context.Context        // cancelled to abort the running prompt
	InstanceID         string
	SessionID          string
	HasSession         bool
	Cwd                string
	Model              string
	Prompt             string
	Emitter            StreamEmitter
	OnSessionID        func(sessionID string) // called when backend reports a session ID
	PermissionResolver PermissionResolver     // nil for backends that use hooks (e.g. Claude)
}

// Backend is the interface that all AI backends must implement.
type Backend interface {
	// ID returns the unique identifier for this backend (e.g. "claude", "opencode").
	ID() string
	// Info returns metadata about the backend and its providers.
	Info() BackendInfo
	// RunPrompt executes a prompt. It blocks until complete and should be called in a goroutine.
	RunPrompt(opts RunOpts) error
	// RefreshProviders re-discovers available providers and models.
	RefreshProviders() error
	// SupportsHooks returns true if this backend supports the Claude Code hook system.
	SupportsHooks() bool
	// BinaryPath returns the path to the backend CLI binary.
	BinaryPath() string
}

// ToolUseEvent is emitted when a backend invokes a tool.
type ToolUseEvent struct {
	InstanceID string          `json:"instance_id"`
	ToolName   string          `json:"tool_name"`
	ToolInput  json.RawMessage `json:"tool_input"`
	ToolUseID  string          `json:"tool_use_id"`
}
