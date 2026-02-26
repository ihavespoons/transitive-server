package instance

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/ihavespoons/claudette-server/internal/protocol"
	"github.com/google/uuid"
)

// EventSink receives protocol envelopes to be sent to the mobile client.
type EventSink func(env *protocol.Envelope)

// Instance is the common interface for all instance types.
type Instance interface {
	ID() string
	SessionID() string
	Cwd() string
	Status() string
	Type() string // "managed" or "attached"
	Stop() error
	Info() protocol.InstanceInfo
}

// Manager tracks all Claude Code instances (managed and attached).
type Manager struct {
	mu        sync.RWMutex
	instances map[string]Instance
	sink      EventSink
}

func NewManager(sink EventSink) *Manager {
	return &Manager{
		instances: make(map[string]Instance),
		sink:      sink,
	}
}

func (m *Manager) emit(msgType string, payload any) {
	if m.sink == nil {
		return
	}
	env, err := protocol.NewEnvelope(msgType, uuid.New().String(), payload)
	if err != nil {
		log.Printf("failed to create envelope: %v", err)
		return
	}
	m.sink(env)
}

func (m *Manager) Add(inst Instance) {
	m.mu.Lock()
	m.instances[inst.ID()] = inst
	m.mu.Unlock()
}

func (m *Manager) Remove(id string) {
	m.mu.Lock()
	delete(m.instances, id)
	m.mu.Unlock()
}

func (m *Manager) Get(id string) Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instances[id]
}

func (m *Manager) List() []protocol.InstanceInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]protocol.InstanceInfo, 0, len(m.instances))
	for _, inst := range m.instances {
		list = append(list, inst.Info())
	}
	return list
}

// LaunchManaged creates a new managed Claude Code instance.
func (m *Manager) LaunchManaged(cwd, model, permissionMode, claudePath string) *ManagedInstance {
	inst := NewManagedInstance(cwd, model, permissionMode, claudePath, m.sink)
	m.Add(inst)
	m.emit(protocol.TypeInstanceLaunched, protocol.InstanceLaunched{
		InstanceID: inst.ID(),
		SessionID:  inst.SessionID(),
		Cwd:        inst.Cwd(),
	})
	return inst
}

// HandleMessage processes an incoming message from the mobile client.
func (m *Manager) HandleMessage(env *protocol.Envelope) {
	log.Printf("HandleMessage: type=%s", env.Type)
	switch env.Type {
	case protocol.TypeInstanceListRequest:
		m.emit(protocol.TypeInstanceList, protocol.InstanceList{
			Instances: m.List(),
		})

	case protocol.TypeInstanceLaunch:
		var req protocol.InstanceLaunchRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			log.Printf("invalid instance.launch payload: %v", err)
			return
		}
		m.LaunchManaged(req.Cwd, req.Model, req.PermissionMode, "claude")

	case protocol.TypeInstanceStop:
		var req protocol.InstanceStopRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			log.Printf("invalid instance.stop payload: %v", err)
			return
		}
		inst := m.Get(req.InstanceID)
		if inst != nil {
			if err := inst.Stop(); err != nil {
				log.Printf("failed to stop instance %s: %v", req.InstanceID, err)
			}
		}

	case protocol.TypeInstanceConfigUpdate:
		var req protocol.InstanceConfigUpdate
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			log.Printf("invalid instance.config.update payload: %v", err)
			return
		}
		inst := m.Get(req.InstanceID)
		if managed, ok := inst.(*ManagedInstance); ok {
			managed.UpdateConfig(req.Model, req.PermissionMode)
			log.Printf("[managed %s] config updated: model=%s, permissionMode=%s", req.InstanceID, req.Model, req.PermissionMode)
		}

	case protocol.TypePromptSend:
		var req protocol.PromptSend
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			log.Printf("invalid prompt.send payload: %v", err)
			return
		}
		inst := m.Get(req.InstanceID)
		if inst == nil {
			log.Printf("instance %s not found", req.InstanceID)
			m.emit(protocol.TypeInstanceStopped, protocol.InstanceStopped{
				InstanceID: req.InstanceID,
				Reason:     "not_found",
			})
			return
		}
		if managed, ok := inst.(*ManagedInstance); ok {
			// Auto-resume stopped instances.
			if managed.Status() == "stopped" {
				log.Printf("resuming stopped instance %s", req.InstanceID)
				if err := managed.Resume(); err != nil {
					log.Printf("failed to resume instance %s: %v", req.InstanceID, err)
					return
				}
				m.emit(protocol.TypeInstanceLaunched, protocol.InstanceLaunched{
					InstanceID: managed.ID(),
					SessionID:  managed.SessionID(),
					Cwd:        managed.Cwd(),
				})
			}
			if err := managed.SendPrompt(req.Text); err != nil {
				log.Printf("failed to send prompt to %s: %v", req.InstanceID, err)
			}
		}
	}
}
