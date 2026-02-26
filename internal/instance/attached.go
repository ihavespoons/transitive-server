package instance

import (
	"sync"

	"github.com/ihavespoons/claudette-server/internal/protocol"
)

// AttachedInstance represents a Claude Code instance discovered via hooks
// (i.e., already running in a terminal, not launched by us).
type AttachedInstance struct {
	id          string
	sessionID   string
	cwd         string
	projectName string
	status      string
	mu          sync.Mutex
}

func NewAttachedInstance(id, sessionID, cwd, projectName string) *AttachedInstance {
	return &AttachedInstance{
		id:          id,
		sessionID:   sessionID,
		cwd:         cwd,
		projectName: projectName,
		status:      "running",
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
	a.mu.Unlock()
}

func (a *AttachedInstance) Info() protocol.InstanceInfo {
	return protocol.InstanceInfo{
		InstanceID: a.id,
		SessionID:  a.sessionID,
		Status:     a.Status(),
		Cwd:        a.cwd,
		Type:       "attached",
	}
}

func (a *AttachedInstance) Stop() error {
	a.SetStatus("stopped")
	return nil
}
