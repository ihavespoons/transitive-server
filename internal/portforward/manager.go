package portforward

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/ihavespoons/transitive/internal/protocol"
)

func bytesReader(b []byte) io.Reader {
	return bytes.NewReader(b)
}

const (
	maxResponseBody = 10 * 1024 * 1024  // 10MB
	chunkSize       = 600 * 1024        // ~600KB per proxy.response
)

// EventSink sends protocol envelopes to the relay.
type EventSink func(env *protocol.Envelope)

// Forward represents an active port forward.
type Forward struct {
	Token      string
	Port       int
	InstanceID string
	ProxyURL   string
}

// Manager manages port forwards across instances.
type Manager struct {
	mu      sync.Mutex
	forwards map[string]*Forward  // token → Forward
	byPort   map[int]*Forward     // port → Forward (dedup)
	byInst   map[string][]*Forward // instanceID → forwards
	sink     EventSink
	relayHost string
	agentID   string
}

func NewManager(sink EventSink, relayHost, agentID string) *Manager {
	return &Manager{
		forwards:  make(map[string]*Forward),
		byPort:    make(map[int]*Forward),
		byInst:    make(map[string][]*Forward),
		sink:      sink,
		relayHost: relayHost,
		agentID:   agentID,
	}
}

func (m *Manager) emit(msgType string, payload any) {
	if m.sink == nil {
		return
	}
	env, err := protocol.NewEnvelope(msgType, uuid.New().String(), payload)
	if err != nil {
		log.Printf("[portforward] failed to create envelope: %v", err)
		return
	}
	m.sink(env)
}

func generateToken() (string, error) {
	b := make([]byte, 16) // 128-bit
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Start begins a port forward for the given instance and port.
func (m *Manager) Start(instanceID string, port int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.byPort[port]; ok {
		if existing.InstanceID != instanceID {
			m.mu.Unlock()
			m.emit(protocol.TypePortForwardError, protocol.PortForwardError{
				InstanceID: instanceID,
				Port:       port,
				Error:      fmt.Sprintf("port %d already forwarded by another instance", port),
			})
			m.mu.Lock()
			return fmt.Errorf("port %d already forwarded", port)
		}
		// Already forwarded by the same instance — no-op.
		return nil
	}

	token, err := generateToken()
	if err != nil {
		return fmt.Errorf("generate token: %w", err)
	}

	proxyURL := fmt.Sprintf("https://%s/proxy/%s/%s/%d/", m.relayHost, m.agentID, token, port)

	fwd := &Forward{
		Token:      token,
		Port:       port,
		InstanceID: instanceID,
		ProxyURL:   proxyURL,
	}

	m.forwards[token] = fwd
	m.byPort[port] = fwd
	m.byInst[instanceID] = append(m.byInst[instanceID], fwd)

	// Tell relay to register this proxy token.
	m.emit(protocol.TypeProxyRegister, protocol.ProxyRegister{
		Token: token,
		Port:  port,
	})

	// Tell mobile the proxy URL is ready.
	m.emit(protocol.TypePortForwardStarted, protocol.PortForwardStarted{
		InstanceID: instanceID,
		Port:       port,
		ProxyURL:   proxyURL,
	})

	log.Printf("[portforward] started: instance=%s port=%d url=%s", instanceID, port, proxyURL)
	return nil
}

// Stop removes a port forward for the given instance and port.
func (m *Manager) Stop(instanceID string, port int) {
	m.mu.Lock()
	fwd, ok := m.byPort[port]
	if !ok || fwd.InstanceID != instanceID {
		m.mu.Unlock()
		return
	}

	delete(m.forwards, fwd.Token)
	delete(m.byPort, port)
	fwds := m.byInst[instanceID]
	for i, f := range fwds {
		if f.Token == fwd.Token {
			m.byInst[instanceID] = append(fwds[:i], fwds[i+1:]...)
			break
		}
	}
	m.mu.Unlock()

	m.emit(protocol.TypeProxyUnregister, protocol.ProxyUnregister{Token: fwd.Token})
	m.emit(protocol.TypePortForwardStopped, protocol.PortForwardStopped{
		InstanceID: instanceID,
		Port:       port,
	})
	log.Printf("[portforward] stopped: instance=%s port=%d", instanceID, port)
}

// StopAllForInstance removes all port forwards for an instance.
func (m *Manager) StopAllForInstance(instanceID string) {
	m.mu.Lock()
	fwds := m.byInst[instanceID]
	delete(m.byInst, instanceID)
	for _, fwd := range fwds {
		delete(m.forwards, fwd.Token)
		delete(m.byPort, fwd.Port)
	}
	m.mu.Unlock()

	for _, fwd := range fwds {
		m.emit(protocol.TypeProxyUnregister, protocol.ProxyUnregister{Token: fwd.Token})
		m.emit(protocol.TypePortForwardStopped, protocol.PortForwardStopped{
			InstanceID: instanceID,
			Port:       fwd.Port,
		})
	}
}

// HandleProxyRequest handles an incoming proxy.request from the relay.
func (m *Manager) HandleProxyRequest(req protocol.ProxyRequest) {
	url := fmt.Sprintf("http://localhost:%d%s", req.Port, req.Path)

	var bodyReader io.Reader
	if req.Body != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.Body)
		if err == nil {
			bodyReader = bytesReader(decoded)
		}
	}

	httpReq, err := http.NewRequest(req.Method, url, bodyReader)
	if err != nil {
		m.sendProxyError(req.ID, 502, "bad request")
		return
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		m.sendProxyError(req.ID, 502, err.Error())
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		m.sendProxyError(req.ID, 502, "read body: "+err.Error())
		return
	}

	headers := make(map[string]string)
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}

	// Chunk the response body.
	if len(body) == 0 {
		m.emit(protocol.TypeProxyResponse, protocol.ProxyResponse{
			ID:         req.ID,
			Status:     resp.StatusCode,
			Headers:    headers,
			BodyBase64: "",
			Seq:        0,
			Done:       true,
		})
		return
	}

	seq := 0
	for offset := 0; offset < len(body); offset += chunkSize {
		end := offset + chunkSize
		if end > len(body) {
			end = len(body)
		}
		chunk := base64.StdEncoding.EncodeToString(body[offset:end])
		done := end >= len(body)

		var hdrs map[string]string
		if seq == 0 {
			hdrs = headers
		}

		m.emit(protocol.TypeProxyResponse, protocol.ProxyResponse{
			ID:         req.ID,
			Status:     resp.StatusCode,
			Headers:    hdrs,
			BodyBase64: chunk,
			Seq:        seq,
			Done:       done,
		})
		seq++
	}
}

func (m *Manager) sendProxyError(reqID string, status int, msg string) {
	m.emit(protocol.TypeProxyResponse, protocol.ProxyResponse{
		ID:         reqID,
		Status:     status,
		Headers:    map[string]string{"Content-Type": "text/plain"},
		BodyBase64: base64.StdEncoding.EncodeToString([]byte(msg)),
		Seq:        0,
		Done:       true,
	})
}
