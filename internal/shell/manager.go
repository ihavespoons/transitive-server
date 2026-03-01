package shell

import (
	"fmt"
	"log"
	"sync"

	"github.com/google/uuid"
	"github.com/ihavespoons/transitive/internal/protocol"
)

// EventSink sends protocol envelopes to the relay.
type EventSink func(env *protocol.Envelope)

// Manager manages one shell session per instance.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session // instanceID → Session
	sink     EventSink
}

func NewManager(sink EventSink) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		sink:     sink,
	}
}

func (m *Manager) emit(msgType string, payload any) {
	if m.sink == nil {
		return
	}
	env, err := protocol.NewEnvelope(msgType, uuid.New().String(), payload)
	if err != nil {
		log.Printf("[shell] failed to create envelope: %v", err)
		return
	}
	m.sink(env)
}

// Start creates a new shell session for an instance.
func (m *Manager) Start(instanceID, cwd string, cols, rows int) error {
	m.mu.Lock()
	if _, exists := m.sessions[instanceID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("shell already active for instance %s", instanceID)
	}
	m.mu.Unlock()

	session, err := NewSession(instanceID, cwd, cols, rows,
		func(instID, data string) {
			m.emit(protocol.TypeShellOutput, protocol.ShellOutput{
				InstanceID: instID,
				Data:       data,
			})
		},
		func(instID, reason string) {
			m.mu.Lock()
			delete(m.sessions, instID)
			m.mu.Unlock()
			m.emit(protocol.TypeShellStopped, protocol.ShellStopped{
				InstanceID: instID,
				Reason:     reason,
			})
		},
	)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.sessions[instanceID] = session
	m.mu.Unlock()

	m.emit(protocol.TypeShellStarted, protocol.ShellStarted{
		InstanceID: instanceID,
	})
	return nil
}

// Input sends data to the shell for an instance.
func (m *Manager) Input(instanceID, data string) error {
	m.mu.Lock()
	s := m.sessions[instanceID]
	m.mu.Unlock()
	if s == nil {
		return fmt.Errorf("no shell for instance %s", instanceID)
	}
	return s.Write(data)
}

// Resize changes the PTY dimensions for an instance's shell.
func (m *Manager) Resize(instanceID string, cols, rows int) {
	m.mu.Lock()
	s := m.sessions[instanceID]
	m.mu.Unlock()
	if s != nil {
		s.Resize(cols, rows)
	}
}

// Stop terminates the shell for an instance.
func (m *Manager) Stop(instanceID string) {
	m.mu.Lock()
	s := m.sessions[instanceID]
	delete(m.sessions, instanceID)
	m.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

// StopAll terminates all shell sessions.
func (m *Manager) StopAll() {
	m.mu.Lock()
	sessions := make(map[string]*Session)
	for k, v := range m.sessions {
		sessions[k] = v
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}
}
