package instance

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ihavespoons/transitive/internal/backend"
	"github.com/ihavespoons/transitive/internal/protocol"
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
	backendID      string
	backend        backend.Backend
	status         string
	sink           EventSink
	mu             sync.Mutex
	busy           bool // true while a prompt is being processed
	hasSession     bool // true after first successful prompt
	onPersist      func() // called when state changes that should be persisted
	repoURL       string
	bootstrapDone chan struct{} // closed when bootstrap completes (success or failure)
	bootstrapErr  error
	permResolver  *backend.ChanPermissionResolver // set for opencode instances
	onPermEmit    func(requestID, instanceID string) // registers requestID with router
	cancelPrompt  context.CancelFunc // cancels the currently running prompt
}

func NewManagedInstance(cwd, model, permissionMode, repoURL string, b backend.Backend, sink EventSink) *ManagedInstance {
	cwd = strings.TrimSpace(cwd)
	if strings.HasPrefix(cwd, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			cwd = home + cwd[1:]
		}
	}

	if repoURL == "" {
		// Only pre-create the directory when there's no repo to clone.
		// When cloning, git will create the target directory itself.
		if err := os.MkdirAll(cwd, 0755); err != nil {
			log.Printf("failed to create working directory %s: %v", cwd, err)
		}
	}

	m := &ManagedInstance{
		id:             uuid.New().String(),
		sessionID:      uuid.New().String(),
		cwd:            cwd,
		model:          model,
		permissionMode: permissionMode,
		backendID:      b.ID(),
		backend:        b,
		status:         "running",
		sink:           sink,
		repoURL:        repoURL,
	}
	if b.ID() == "opencode" {
		m.permResolver = backend.NewChanPermissionResolver()
	}
	if repoURL != "" {
		m.bootstrapDone = make(chan struct{})
	}
	return m
}

// NewManagedInstanceFromSession creates a ManagedInstance that resumes an existing session.
// Used when promoting an AttachedInstance so it can receive prompts.
func NewManagedInstanceFromSession(id, sessionID, cwd string, b backend.Backend, sink EventSink) *ManagedInstance {
	return &ManagedInstance{
		id:         id,
		sessionID:  sessionID,
		cwd:        cwd,
		backendID:  b.ID(),
		backend:    b,
		status:     "running",
		sink:       sink,
		hasSession: true,
	}
}

// NewManagedInstanceFromPersisted creates a stopped ManagedInstance from persisted state.
func NewManagedInstanceFromPersisted(p PersistedInstance, b backend.Backend, sink EventSink) *ManagedInstance {
	return &ManagedInstance{
		id:             p.ID,
		sessionID:      p.SessionID,
		cwd:            p.Cwd,
		model:          p.Model,
		permissionMode: p.PermissionMode,
		backendID:      b.ID(),
		backend:        b,
		status:         "stopped",
		sink:           sink,
		hasSession:     p.HasSession,
	}
}

// Persist exports the current instance state for serialization.
func (m *ManagedInstance) Persist() PersistedInstance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return PersistedInstance{
		ID:             m.id,
		SessionID:      m.sessionID,
		Cwd:            m.cwd,
		Model:          m.model,
		PermissionMode: m.permissionMode,
		HasSession:     m.hasSession,
		BackendID:      m.backendID,
	}
}

func (m *ManagedInstance) ID() string        { return m.id }
func (m *ManagedInstance) SessionID() string  { return m.sessionID }
func (m *ManagedInstance) Cwd() string        { return m.cwd }
func (m *ManagedInstance) Type() string       { return "managed" }
func (m *ManagedInstance) BackendID() string  { return m.backendID }

func (m *ManagedInstance) Status() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *ManagedInstance) Model() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.model
}

func (m *ManagedInstance) PermissionMode() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.permissionMode
}

func (m *ManagedInstance) Info() protocol.InstanceInfo {
	return protocol.InstanceInfo{
		InstanceID: m.id,
		SessionID:  m.sessionID,
		Status:     m.Status(),
		Cwd:        m.cwd,
		Type:       "managed",
		BackendID:  m.backendID,
	}
}

func (m *ManagedInstance) Stop() error {
	m.mu.Lock()
	m.status = "stopped"
	cancel := m.cancelPrompt
	m.mu.Unlock()

	// Cancel the running prompt first.
	if cancel != nil {
		cancel()
	}

	// Stop the OpenCode server process if this is an opencode backend.
	if ocb, ok := m.backend.(*backend.OpenCodeBackend); ok {
		ocb.StopServer(m.id)
	}
	return nil
}

// CancelPrompt aborts the currently running prompt without stopping the instance.
func (m *ManagedInstance) CancelPrompt() {
	m.mu.Lock()
	cancel := m.cancelPrompt
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
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

// StartBootstrap kicks off the clone in a background goroutine.
// Call this right after creating the instance so the UI sees progress immediately.
func (m *ManagedInstance) StartBootstrap() {
	if m.bootstrapDone == nil {
		return
	}
	go m.bootstrap()
}

// bootstrap clones the repository into the instance's working directory.
func (m *ManagedInstance) bootstrap() {
	defer close(m.bootstrapDone)

	m.emit(protocol.TypeNotification, protocol.Notification{
		InstanceID: m.id,
		Type:       "bootstrap",
		Message:    m.repoURL,
	})

	// Ensure the parent directory exists; git clone will create m.cwd itself.
	if err := os.MkdirAll(filepath.Dir(m.cwd), 0755); err != nil {
		m.bootstrapErr = fmt.Errorf("failed to create parent directory: %w", err)
		m.emit(protocol.TypeNotification, protocol.Notification{
			InstanceID: m.id,
			Type:       "error",
			Message:    fmt.Sprintf("Failed to create directory: %v", err),
		})
		return
	}

	cmd := exec.Command("git", "clone", m.repoURL, m.cwd)

	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		m.emit(protocol.TypeNotification, protocol.Notification{
			InstanceID: m.id,
			Type:       "bootstrap",
			Message:    string(output),
		})
	}

	if err != nil {
		m.bootstrapErr = fmt.Errorf("git clone failed: %w", err)
		m.emit(protocol.TypeNotification, protocol.Notification{
			InstanceID: m.id,
			Type:       "error",
			Message:    fmt.Sprintf("Failed to clone repository: %v", err),
		})
		log.Printf("[managed %s] bootstrap failed: %v", m.id, err)
		return
	}

	m.emit(protocol.TypeNotification, protocol.Notification{
		InstanceID: m.id,
		Type:       "bootstrap_complete",
		Message:    "Repository cloned successfully",
	})
	log.Printf("[managed %s] bootstrap complete: %s", m.id, m.repoURL)
}

// SendPrompt spawns a backend process for this message, streams output, then exits.
func (m *ManagedInstance) SendPrompt(text string) error {
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return fmt.Errorf("instance is busy processing another prompt")
	}
	m.busy = true
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelPrompt = cancel
	m.mu.Unlock()

	go m.runPrompt(ctx, text)
	return nil
}

func (m *ManagedInstance) runPrompt(ctx context.Context, text string) {
	defer func() {
		m.mu.Lock()
		m.busy = false
		m.cancelPrompt = nil
		m.mu.Unlock()
	}()

	// Wait for bootstrap (clone) to finish before running the prompt.
	if m.bootstrapDone != nil {
		<-m.bootstrapDone
		if m.bootstrapErr != nil {
			m.emit(protocol.TypeNotification, protocol.Notification{
				InstanceID: m.id,
				Type:       "error",
				Message:    fmt.Sprintf("Cannot run prompt: repository clone failed: %v", m.bootstrapErr),
			})
			return
		}
	}

	m.mu.Lock()
	hasSession := m.hasSession
	model := m.model
	m.mu.Unlock()

	opts := backend.RunOpts{
		Ctx:        ctx,
		InstanceID: m.id,
		SessionID:  m.sessionID,
		HasSession: hasSession,
		Cwd:        m.cwd,
		Model:      model,
		Prompt:     text,
		Emitter:    m.emitFromBackend,
		OnSessionID: func(sessionID string) {
			m.mu.Lock()
			wasNew := !m.hasSession
			m.sessionID = sessionID
			m.hasSession = true
			cb := m.onPersist
			m.mu.Unlock()
			if wasNew && cb != nil {
				cb()
			}
		},
		PermissionResolver: m.permResolver,
	}

	if err := m.backend.RunPrompt(opts); err != nil {
		log.Printf("[managed %s] backend.RunPrompt error: %v", m.id, err)
	}

	// If the prompt was cancelled (context done), emit instance.stopped so the
	// iOS UI resets streaming/thinking state.
	if ctx.Err() != nil {
		m.emit(protocol.TypeInstanceStopped, protocol.InstanceStopped{
			InstanceID: m.id,
			Reason:     "turn_complete",
		})
	}
}

// emitFromBackend is the StreamEmitter callback used by backends.
// It also intercepts system events to update local model/permissionMode state,
// and registers permission/question requestIDs with the router for OpenCode instances.
func (m *ManagedInstance) emitFromBackend(msgType string, payload any) {
	// Intercept instance.status to update local state.
	if msgType == protocol.TypeInstanceStatus {
		if status, ok := payload.(protocol.InstanceStatusMsg); ok {
			m.mu.Lock()
			if status.Model != "" {
				m.model = status.Model
			}
			if status.PermissionMode != "" {
				m.permissionMode = status.PermissionMode
			}
			m.mu.Unlock()
		}
	}
	// Register permission/question requestIDs with the router so responses
	// can be routed back to this instance's PermissionResolver.
	if m.onPermEmit != nil {
		if msgType == protocol.TypePermissionRequest {
			if req, ok := payload.(protocol.PermissionRequestMsg); ok {
				m.onPermEmit(req.RequestID, m.id)
			}
		} else if msgType == protocol.TypeAskUserQuestion {
			if req, ok := payload.(protocol.AskUserQuestion); ok {
				m.onPermEmit(req.RequestID, m.id)
			}
		}
	}
	m.emit(msgType, payload)
}

// SetOnPermEmit sets the callback for registering permission request IDs with the router.
func (m *ManagedInstance) SetOnPermEmit(fn func(requestID, instanceID string)) {
	m.mu.Lock()
	m.onPermEmit = fn
	m.mu.Unlock()
}

// ResolvePermission forwards a permission/question response to the OpenCode server
// via the ChanPermissionResolver.
func (m *ManagedInstance) ResolvePermission(requestID string, response map[string]any) {
	if m.permResolver != nil {
		m.permResolver.Resolve(requestID, response)
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
