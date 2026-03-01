package instance

import (
	"encoding/json"
	"log"
	"net/url"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ihavespoons/transitive/internal/backend"
	"github.com/ihavespoons/transitive/internal/protocol"
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
	mu         sync.RWMutex
	instances  map[string]Instance
	sink       EventSink
	registry   *backend.Registry
	projectDir string
	onPermEmit func(requestID, instanceID string) // routes permission requestIDs to instances
}

func NewManager(sink EventSink, registry *backend.Registry, projectDir string) *Manager {
	return &Manager{
		instances:  make(map[string]Instance),
		sink:       sink,
		registry:   registry,
		projectDir: projectDir,
	}
}

// repoNameFromURL extracts the repository name from a URL or owner/repo string.
func repoNameFromURL(repoURL string) string {
	parsed, err := url.Parse(repoURL)
	if err != nil {
		return ""
	}
	name := path.Base(parsed.Path)
	name = strings.TrimSuffix(name, ".git")
	return name
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

// SetOnPermEmit sets the callback used to register permission requestIDs
// with the router. Must be called before LoadPersisted or LaunchManaged.
func (m *Manager) SetOnPermEmit(fn func(requestID, instanceID string)) {
	m.onPermEmit = fn
}

// setOnPersist wires the onPersist callback on a ManagedInstance so that
// hasSession flips trigger Save().
func (m *Manager) setOnPersist(inst *ManagedInstance) {
	inst.mu.Lock()
	inst.onPersist = func() { m.Save() }
	inst.mu.Unlock()
}

// wirePermEmit wires the onPermEmit callback on a ManagedInstance
// if the manager has one configured.
func (m *Manager) wirePermEmit(inst *ManagedInstance) {
	if m.onPermEmit != nil {
		inst.SetOnPermEmit(m.onPermEmit)
	}
}

// Save persists all managed instances to disk.
func (m *Manager) Save() {
	m.mu.RLock()
	var persisted []PersistedInstance
	for _, inst := range m.instances {
		if managed, ok := inst.(*ManagedInstance); ok {
			persisted = append(persisted, managed.Persist())
		}
	}
	m.mu.RUnlock()

	if err := saveStore(persisted); err != nil {
		log.Printf("failed to persist instances: %v", err)
	}
}

// LoadPersisted restores managed instances from disk as stopped instances.
func (m *Manager) LoadPersisted() {
	entries, err := loadStore()
	if err != nil {
		log.Printf("failed to load persisted instances: %v", err)
		return
	}
	m.mu.Lock()
	for _, p := range entries {
		backendID := p.BackendID
		if backendID == "" {
			backendID = "claude"
		}
		b := m.registry.Get(backendID)
		if b == nil {
			log.Printf("skipping persisted instance %s: backend %q not found", p.ID, backendID)
			continue
		}
		inst := NewManagedInstanceFromPersisted(p, b, m.sink)
		inst.onPersist = func() { m.Save() }
		m.wirePermEmit(inst)
		m.instances[inst.ID()] = inst
	}
	m.mu.Unlock()
	if len(entries) > 0 {
		log.Printf("loaded %d persisted instance(s)", len(entries))
	}
}

// Registry returns the backend registry.
func (m *Manager) Registry() *backend.Registry {
	return m.registry
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
	m.Save()
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

// LaunchManaged creates a new managed instance using the specified backend.
func (m *Manager) LaunchManaged(cwd, model, permissionMode, backendID, repoURL string) *ManagedInstance {
	if backendID == "" {
		backendID = "claude"
	}
	b := m.registry.Get(backendID)
	if b == nil {
		log.Printf("backend %q not found, falling back to claude", backendID)
		b = m.registry.Get("claude")
		if b == nil {
			log.Printf("no backends available")
			return nil
		}
	}

	inst := NewManagedInstance(cwd, model, permissionMode, repoURL, b, m.sink)
	m.setOnPersist(inst)
	m.wirePermEmit(inst)
	m.Add(inst)
	m.emit(protocol.TypeInstanceLaunched, protocol.InstanceLaunched{
		InstanceID:     inst.ID(),
		SessionID:      inst.SessionID(),
		Cwd:            inst.Cwd(),
		Model:          inst.Model(),
		PermissionMode: inst.PermissionMode(),
		BackendID:      inst.BackendID(),
	})
	// Start cloning immediately so the UI sees progress right away.
	inst.StartBootstrap()
	m.Save()
	return inst
}

// HandleMessage processes an incoming message from the mobile client.
func (m *Manager) HandleMessage(env *protocol.Envelope) {
	log.Printf("HandleMessage: type=%s", env.Type)
	switch env.Type {
	case protocol.TypeInstanceListRequest:
		m.emit(protocol.TypeServerConfig, protocol.ServerConfig{
			ProjectDir: m.projectDir,
		})
		m.emit(protocol.TypeInstanceList, protocol.InstanceList{
			Instances: m.List(),
		})

	case protocol.TypeBackendListRequest:
		m.sendBackendList()

	case protocol.TypeInstanceLaunch:
		var req protocol.InstanceLaunchRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			log.Printf("invalid instance.launch payload: %v", err)
			return
		}
		cwd := req.Cwd
		if req.RepositoryURL != "" {
			if name := repoNameFromURL(req.RepositoryURL); name != "" {
				cwd = m.projectDir + "/" + name
			}
		}
		m.LaunchManaged(cwd, req.Model, req.PermissionMode, req.BackendID, req.RepositoryURL)

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

	case protocol.TypeInstanceRemove:
		var req protocol.InstanceRemoveRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			log.Printf("invalid instance.remove payload: %v", err)
			return
		}
		inst := m.Get(req.InstanceID)
		if inst != nil {
			if err := inst.Stop(); err != nil {
				log.Printf("failed to stop instance %s: %v", req.InstanceID, err)
			}
			m.Remove(req.InstanceID)
			log.Printf("removed instance %s", req.InstanceID)
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
			m.Save()
		} else if attached, ok := inst.(*AttachedInstance); ok {
			if req.PermissionMode != "" {
				attached.SetPermissionMode(req.PermissionMode)
			}
			log.Printf("[attached %s] config updated: permissionMode=%s", req.InstanceID, req.PermissionMode)
		}

	case protocol.TypeInstanceAdopt:
		var req protocol.InstanceAdoptRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			log.Printf("invalid instance.adopt payload: %v", err)
			return
		}
		if adopted := m.adoptAttached(req.InstanceID); adopted != nil {
			m.Save()
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
		managed, ok := inst.(*ManagedInstance)
		if !ok {
			managed = m.adoptAttached(req.InstanceID)
		}
		if managed != nil {
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
					BackendID:  managed.BackendID(),
				})
			}
			if err := managed.SendPrompt(req.Text); err != nil {
				log.Printf("failed to send prompt to %s: %v", req.InstanceID, err)
			}
		}
	}
}

func (m *Manager) sendBackendList() {
	backends := m.registry.List()
	msgs := make([]protocol.BackendInfoMsg, 0, len(backends))
	for _, b := range backends {
		providers := make([]protocol.ProviderInfoMsg, 0, len(b.Providers))
		for _, p := range b.Providers {
			models := p.Models
			if models == nil {
				models = []string{}
			}
			providers = append(providers, protocol.ProviderInfoMsg{
				ID:     p.ID,
				Name:   p.Name,
				Models: models,
			})
		}
		msgs = append(msgs, protocol.BackendInfoMsg{
			ID:        b.ID,
			Name:      b.Name,
			Available: b.Available,
			Providers: providers,
		})
	}
	m.emit(protocol.TypeBackendList, protocol.BackendListMsg{Backends: msgs})
}

// adoptAttached signals the terminal CLI to stop and promotes the attached instance to managed.
func (m *Manager) adoptAttached(instanceID string) *ManagedInstance {
	inst := m.Get(instanceID)
	if inst == nil {
		return nil
	}
	// Already managed — nothing to do.
	if managed, ok := inst.(*ManagedInstance); ok {
		return managed
	}
	attached, ok := inst.(*AttachedInstance)
	if !ok {
		return nil
	}

	log.Printf("adopting attached instance %s", instanceID)
	if pid := attached.PID(); pid > 0 {
		log.Printf("sending SIGINT to CLI (PID %d) for instance %s", pid, instanceID)
		syscall.Kill(pid, syscall.SIGINT)
		if attached.WaitStopped(5 * time.Second) {
			log.Printf("attached instance %s stopped successfully", instanceID)
		} else {
			log.Printf("attached instance %s did not stop in time, proceeding anyway", instanceID)
		}
	}

	// Use the backend reported by the attached instance, defaulting to "claude".
	backendID := attached.BackendID()
	if backendID == "" {
		backendID = "claude"
	}
	b := m.registry.Get(backendID)
	if b == nil {
		log.Printf("backend %q not found, cannot adopt instance %s", backendID, instanceID)
		return nil
	}

	managed := NewManagedInstanceFromSession(attached.ID(), attached.SessionID(), attached.Cwd(), b, m.sink)
	m.setOnPersist(managed)
	m.wirePermEmit(managed)
	m.Add(managed) // replaces attached in the map
	m.emit(protocol.TypeInstanceLaunched, protocol.InstanceLaunched{
		InstanceID: managed.ID(),
		SessionID:  managed.SessionID(),
		Cwd:        managed.Cwd(),
		BackendID:  managed.BackendID(),
	})
	return managed
}
