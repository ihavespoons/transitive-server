package bgprocess

import (
	"encoding/base64"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ihavespoons/transitive/internal/protocol"
)

// EventSink sends protocol envelopes to the relay.
type EventSink func(env *protocol.Envelope)

// Manager manages background processes across instances.
type Manager struct {
	mu          sync.Mutex
	processes   map[string]*Process   // processID → Process
	byInstance  map[string][]*Process // instanceID → processes
	sink        EventSink

	// OnStopped is called when a process exits (any reason). Set by the
	// caller to clean up associated resources like port forwards.
	OnStopped func(instanceID string, port int)

	// Output batching: collect output per process, flush periodically.
	batchMu     sync.Mutex
	batchBufs   map[string][]byte // processID → pending output
	batchTicker *time.Ticker
	batchDone   chan struct{}
}

// NewManager creates a new background process manager.
func NewManager(sink EventSink) *Manager {
	m := &Manager{
		processes:  make(map[string]*Process),
		byInstance: make(map[string][]*Process),
		sink:       sink,
		batchBufs:  make(map[string][]byte),
		batchDone:  make(chan struct{}),
	}
	m.batchTicker = time.NewTicker(200 * time.Millisecond)
	go m.batchLoop()
	return m
}

func (m *Manager) emit(msgType string, payload any) {
	if m.sink == nil {
		return
	}
	env, err := protocol.NewEnvelope(msgType, uuid.New().String(), payload)
	if err != nil {
		log.Printf("[bgprocess] failed to create envelope: %v", err)
		return
	}
	m.sink(env)
}

// Start launches a new background process.
func (m *Manager) Start(instanceID, name, command, cwd string, port int) (string, error) {
	processID := uuid.New().String()
	p := &Process{
		ID:         processID,
		InstanceID: instanceID,
		Name:       name,
		Command:    command,
		Cwd:        cwd,
		Port:       port,
	}

	if err := p.Start(); err != nil {
		m.emit(protocol.TypeBgProcessError, protocol.BackgroundProcessError{
			InstanceID: instanceID,
			Error:      fmt.Sprintf("failed to start process: %v", err),
		})
		return "", err
	}

	// Set up output capture with batching.
	p.buf.SetOnWrite(func(data []byte) {
		m.batchMu.Lock()
		m.batchBufs[processID] = append(m.batchBufs[processID], data...)
		// Flush immediately if batch exceeds 4KB.
		if len(m.batchBufs[processID]) >= 4096 {
			buf := m.batchBufs[processID]
			delete(m.batchBufs, processID)
			m.batchMu.Unlock()
			m.emitOutput(instanceID, processID, buf)
			return
		}
		m.batchMu.Unlock()
	})

	m.mu.Lock()
	m.processes[processID] = p
	m.byInstance[instanceID] = append(m.byInstance[instanceID], p)
	m.mu.Unlock()

	m.emit(protocol.TypeBgProcessStarted, protocol.BackgroundProcessStarted{
		InstanceID: instanceID,
		ProcessID:  processID,
		Name:       name,
		Command:    command,
		Pid:        p.Pid,
		Port:       port,
	})

	// Monitor for exit.
	go func() {
		<-p.Done()
		m.flushOutput(processID)
		reason := "exited"
		errMsg := ""
		p.mu.Lock()
		if p.Status == "errored" {
			reason = "error"
			errMsg = p.ExitError
		}
		p.mu.Unlock()
		// Include error detail and last output so the agent/UI knows what went wrong.
		lastOutput := ""
		if reason == "error" {
			if out := p.Output(); len(out) > 0 {
				lastOutput = string(out)
			}
		}
		m.emit(protocol.TypeBgProcessStopped, protocol.BackgroundProcessStopped{
			InstanceID: instanceID,
			ProcessID:  processID,
			Reason:     reason,
			Error:      errMsg,
			Output:     lastOutput,
		})
		if port > 0 && m.OnStopped != nil {
			m.OnStopped(instanceID, port)
		}
	}()

	return processID, nil
}

// GetProcess returns the Process for a given ID, or nil if not found.
func (m *Manager) GetProcess(processID string) *Process {
	m.mu.Lock()
	p := m.processes[processID]
	m.mu.Unlock()
	return p
}

// FindRecent returns a running process for this instance with the same command
// and port that was started within the last 5 seconds. Used to dedup rapid
// duplicate start requests from the OpenCode tool pipeline.
func (m *Manager) FindRecent(instanceID, command string, port int) *Process {
	m.mu.Lock()
	procs := m.byInstance[instanceID]
	m.mu.Unlock()

	for _, p := range procs {
		if p.Command == command && p.Port == port && time.Since(p.StartedAt) < 5*time.Second {
			return p
		}
	}
	return nil
}

// Stop terminates a background process.
func (m *Manager) Stop(processID string) error {
	m.mu.Lock()
	p, ok := m.processes[processID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("process %s not found", processID)
	}

	p.Stop()

	m.emit(protocol.TypeBgProcessStopped, protocol.BackgroundProcessStopped{
		InstanceID: p.InstanceID,
		ProcessID:  processID,
		Reason:     "stopped",
	})

	if p.Port > 0 && m.OnStopped != nil {
		m.OnStopped(p.InstanceID, p.Port)
	}

	return nil
}

// Restart stops then starts a process with the same parameters.
func (m *Manager) Restart(processID string) (string, error) {
	m.mu.Lock()
	p, ok := m.processes[processID]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("process %s not found", processID)
	}

	instanceID := p.InstanceID
	name := p.Name
	command := p.Command
	cwd := p.Cwd
	port := p.Port

	p.Stop()
	m.removeProcess(processID)

	return m.Start(instanceID, name, command, cwd, port)
}

// List returns info about all processes for an instance.
func (m *Manager) List(instanceID string) []protocol.BackgroundProcessInfo {
	m.mu.Lock()
	procs := m.byInstance[instanceID]
	m.mu.Unlock()

	result := make([]protocol.BackgroundProcessInfo, 0, len(procs))
	for _, p := range procs {
		result = append(result, protocol.BackgroundProcessInfo{
			ProcessID: p.ID,
			Name:      p.Name,
			Command:   p.Command,
			Port:      p.Port,
			Pid:       p.Pid,
			Status:    p.Status,
			StartedAt: p.StartedAt.UnixMilli(),
		})
	}
	return result
}

// StreamOutput sends the current output buffer for a process.
func (m *Manager) StreamOutput(processID string) {
	m.mu.Lock()
	p, ok := m.processes[processID]
	m.mu.Unlock()
	if !ok {
		return
	}

	data := p.Output()
	if len(data) > 0 {
		m.emitOutput(p.InstanceID, processID, data)
	}
}

// StopAllForInstance stops all processes belonging to an instance.
func (m *Manager) StopAllForInstance(instanceID string) {
	m.mu.Lock()
	procs := make([]*Process, len(m.byInstance[instanceID]))
	copy(procs, m.byInstance[instanceID])
	m.mu.Unlock()

	for _, p := range procs {
		if p.IsRunning() {
			p.Stop()
		}
		m.removeProcess(p.ID)
	}

	m.mu.Lock()
	delete(m.byInstance, instanceID)
	m.mu.Unlock()
}

// StopAll stops all background processes.
func (m *Manager) StopAll() {
	m.batchTicker.Stop()
	close(m.batchDone)

	m.mu.Lock()
	procs := make([]*Process, 0, len(m.processes))
	for _, p := range m.processes {
		procs = append(procs, p)
	}
	m.mu.Unlock()

	for _, p := range procs {
		if p.IsRunning() {
			p.Stop()
		}
	}
}

func (m *Manager) removeProcess(processID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.processes[processID]
	if !ok {
		return
	}

	delete(m.processes, processID)

	// Remove from byInstance slice.
	procs := m.byInstance[p.InstanceID]
	for i, proc := range procs {
		if proc.ID == processID {
			m.byInstance[p.InstanceID] = append(procs[:i], procs[i+1:]...)
			break
		}
	}

	// Clean up batch buffer.
	m.batchMu.Lock()
	delete(m.batchBufs, processID)
	m.batchMu.Unlock()
}

func (m *Manager) emitOutput(instanceID, processID string, data []byte) {
	m.emit(protocol.TypeBgProcessOutput, protocol.BackgroundProcessOutput{
		InstanceID: instanceID,
		ProcessID:  processID,
		Data:       base64.StdEncoding.EncodeToString(data),
	})
}

func (m *Manager) flushOutput(processID string) {
	m.batchMu.Lock()
	buf, ok := m.batchBufs[processID]
	if ok {
		delete(m.batchBufs, processID)
	}
	m.batchMu.Unlock()

	if len(buf) > 0 {
		m.mu.Lock()
		p := m.processes[processID]
		m.mu.Unlock()
		if p != nil {
			m.emitOutput(p.InstanceID, processID, buf)
		}
	}
}

func (m *Manager) batchLoop() {
	for {
		select {
		case <-m.batchDone:
			return
		case <-m.batchTicker.C:
			m.batchMu.Lock()
			pending := make(map[string][]byte, len(m.batchBufs))
			for id, buf := range m.batchBufs {
				pending[id] = buf
			}
			m.batchBufs = make(map[string][]byte)
			m.batchMu.Unlock()

			for processID, buf := range pending {
				m.mu.Lock()
				p := m.processes[processID]
				m.mu.Unlock()
				if p != nil {
					m.emitOutput(p.InstanceID, processID, buf)
				}
			}
		}
	}
}
