package instance

import (
	"sync"
	"time"

	"github.com/ihavespoons/transitive/internal/protocol"
)

// AttachedInstance represents a Claude Code instance discovered via hooks
// (i.e., already running in a terminal, not launched by us).
type AttachedInstance struct {
	id             string
	sessionID      string
	cwd            string
	projectName    string
	status         string
	permissionMode string
	backendID      string
	pid            int
	stoppedCh      chan struct{}
	mu             sync.Mutex
}

func NewAttachedInstance(id, sessionID, cwd, projectName string) *AttachedInstance {
	return &AttachedInstance{
		id:          id,
		sessionID:   sessionID,
		cwd:         cwd,
		projectName: projectName,
		status:      "running",
		stoppedCh:   make(chan struct{}),
	}
}

func (a *AttachedInstance) ID() string        { return a.id }
func (a *AttachedInstance) SessionID() string  { return a.sessionID }
func (a *AttachedInstance) Cwd() string        { return a.cwd }
func (a *AttachedInstance) Type() string       { return "attached" }

func (a *AttachedInstance) Status() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

func (a *AttachedInstance) SetStatus(s string) {
	a.mu.Lock()
	a.status = s
	if s == "stopped" {
		select {
		case <-a.stoppedCh:
		default:
			close(a.stoppedCh)
		}
	}
	a.mu.Unlock()
}

func (a *AttachedInstance) PermissionMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.permissionMode
}

func (a *AttachedInstance) SetPermissionMode(mode string) {
	a.mu.Lock()
	a.permissionMode = mode
	a.mu.Unlock()
}

func (a *AttachedInstance) BackendID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.backendID
}

func (a *AttachedInstance) SetBackendID(id string) {
	a.mu.Lock()
	a.backendID = id
	a.mu.Unlock()
}

func (a *AttachedInstance) SetPID(pid int) {
	a.mu.Lock()
	a.pid = pid
	a.mu.Unlock()
}

func (a *AttachedInstance) PID() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pid
}

// WaitStopped blocks until the instance is stopped or the timeout elapses.
func (a *AttachedInstance) WaitStopped(timeout time.Duration) bool {
	select {
	case <-a.stoppedCh:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (a *AttachedInstance) Info() protocol.InstanceInfo {
	return protocol.InstanceInfo{
		InstanceID: a.id,
		SessionID:  a.sessionID,
		Status:     a.Status(),
		Cwd:        a.cwd,
		Type:       "attached",
		BackendID:  a.BackendID(),
	}
}

func (a *AttachedInstance) Stop() error {
	a.SetStatus("stopped")
	return nil
}
