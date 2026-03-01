package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ihavespoons/transitive/internal/protocol"
)

// OpenCodeBackend implements Backend for the OpenCode CLI using the server API.
type OpenCodeBackend struct {
	mu        sync.RWMutex
	path      string
	available bool
	providers []ProviderInfo
	servers   map[string]*ocServer
}

// ocServer tracks a running `opencode serve` process.
type ocServer struct {
	cmd     *exec.Cmd
	port    int
	baseURL string
	ready   chan struct{} // closed when health check passes
	done    chan struct{} // closed when process exits
	err     error        // set if server failed to start
}

// NewOpenCodeBackend creates a new OpenCode backend.
func NewOpenCodeBackend(opencodePath string) *OpenCodeBackend {
	b := &OpenCodeBackend{
		path:    opencodePath,
		servers: make(map[string]*ocServer),
	}
	b.checkAvailability()
	return b
}

func (b *OpenCodeBackend) checkAvailability() {
	_, err := exec.LookPath(b.path)
	b.mu.Lock()
	b.available = err == nil
	b.mu.Unlock()
}

func (b *OpenCodeBackend) ID() string { return "opencode" }

func (b *OpenCodeBackend) Info() BackendInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return BackendInfo{
		ID:        "opencode",
		Name:      "OpenCode",
		Available: b.available,
		Providers: b.providers,
	}
}

func (b *OpenCodeBackend) BinaryPath() string { return b.path }

func (b *OpenCodeBackend) SupportsHooks() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	pluginPath := filepath.Join(home, ".config", "opencode", "plugins", "transitive.ts")
	_, err = os.Stat(pluginPath)
	return err == nil
}

// RefreshProviders reads OpenCode config files to discover enabled providers and models.
func (b *OpenCodeBackend) RefreshProviders() error {
	b.checkAvailability()

	providers := discoverOpenCodeProviders()
	b.mu.Lock()
	b.providers = providers
	b.mu.Unlock()
	return nil
}

// getOrStartServer returns an existing server for the instance or starts a new one.
func (b *OpenCodeBackend) getOrStartServer(instanceID, cwd string) (*ocServer, error) {
	b.mu.Lock()
	if srv, ok := b.servers[instanceID]; ok {
		b.mu.Unlock()
		<-srv.ready
		if srv.err != nil {
			return nil, srv.err
		}
		return srv, nil
	}

	port, err := findFreePort()
	if err != nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("find free port: %w", err)
	}

	srv := &ocServer{
		port:    port,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		ready:   make(chan struct{}),
		done:    make(chan struct{}),
	}

	cmd := exec.Command(b.path, "serve", "--port", fmt.Sprintf("%d", port))
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TRANSITIVE_INSTANCE_ID="+instanceID)

	// Capture stderr for logging.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	log.Printf("[opencode %s] starting: opencode serve --port %d", instanceID, port)
	if err := cmd.Start(); err != nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("start opencode serve: %w", err)
	}
	srv.cmd = cmd

	b.servers[instanceID] = srv
	b.mu.Unlock()

	// Read stderr in background.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[opencode %s stderr] %s", instanceID, scanner.Text())
		}
	}()

	// Wait for process exit in background.
	go func() {
		err := cmd.Wait()
		if err != nil {
			log.Printf("[opencode %s] server process exited: %v", instanceID, err)
		} else {
			log.Printf("[opencode %s] server process exited cleanly", instanceID)
		}
		close(srv.done)
	}()

	// Poll health endpoint until ready.
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		deadline := time.After(30 * time.Second)
		for {
			select {
			case <-srv.done:
				srv.err = fmt.Errorf("server process exited before becoming ready")
				close(srv.ready)
				return
			case <-deadline:
				srv.err = fmt.Errorf("server did not become ready within 30s")
				close(srv.ready)
				return
			default:
			}
			resp, err := client.Get(srv.baseURL + "/global/health")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					log.Printf("[opencode %s] server ready on port %d", instanceID, port)
					close(srv.ready)
					return
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	<-srv.ready
	if srv.err != nil {
		b.mu.Lock()
		delete(b.servers, instanceID)
		b.mu.Unlock()
		return nil, srv.err
	}
	return srv, nil
}

// StopServer stops the opencode serve process for the given instance.
func (b *OpenCodeBackend) StopServer(instanceID string) {
	b.mu.Lock()
	srv, ok := b.servers[instanceID]
	if ok {
		delete(b.servers, instanceID)
	}
	b.mu.Unlock()
	if !ok || srv.cmd == nil || srv.cmd.Process == nil {
		return
	}

	log.Printf("[opencode %s] stopping server", instanceID)
	srv.cmd.Process.Signal(os.Interrupt)

	select {
	case <-srv.done:
	case <-time.After(5 * time.Second):
		log.Printf("[opencode %s] server did not stop gracefully, killing", instanceID)
		srv.cmd.Process.Kill()
		<-srv.done
	}
}

// RunPrompt executes a prompt via the OpenCode server API.
func (b *OpenCodeBackend) RunPrompt(opts RunOpts) error {
	srv, err := b.getOrStartServer(opts.InstanceID, opts.Cwd)
	if err != nil {
		opts.Emitter(protocol.TypeNotification, protocol.Notification{
			InstanceID: opts.InstanceID,
			Type:       "error",
			Message:    fmt.Sprintf("failed to start opencode server: %v", err),
		})
		return fmt.Errorf("start server: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}

	// Create or reuse session.
	sessionID := opts.SessionID
	if !opts.HasSession {
		sid, err := b.createSession(client, srv, opts)
		if err != nil {
			opts.Emitter(protocol.TypeNotification, protocol.Notification{
				InstanceID: opts.InstanceID,
				Type:       "error",
				Message:    fmt.Sprintf("failed to create session: %v", err),
			})
			return fmt.Errorf("create session: %w", err)
		}
		sessionID = sid
	}

	if opts.OnSessionID != nil {
		opts.OnSessionID(sessionID)
	}

	// Connect to SSE event stream.
	// Derive from opts.Ctx so cancelling the prompt aborts SSE processing.
	ctx, cancel := context.WithCancel(opts.Ctx)
	defer cancel()

	sseReq, err := http.NewRequestWithContext(ctx, "GET", srv.baseURL+"/event", nil)
	if err != nil {
		return fmt.Errorf("create SSE request: %w", err)
	}
	sseReq.Header.Set("Accept", "text/event-stream")

	sseClient := &http.Client{} // no timeout for SSE
	sseResp, err := sseClient.Do(sseReq)
	if err != nil {
		return fmt.Errorf("connect SSE: %w", err)
	}
	defer sseResp.Body.Close()

	// Send prompt asynchronously.
	go func() {
		if err := b.sendPrompt(client, srv, sessionID, opts); err != nil {
			log.Printf("[opencode %s] failed to send prompt: %v", opts.InstanceID, err)
			opts.Emitter(protocol.TypeNotification, protocol.Notification{
				InstanceID: opts.InstanceID,
				Type:       "error",
				Message:    fmt.Sprintf("failed to send prompt: %v", err),
			})
			cancel()
		}
	}()

	// Process SSE events. Completion is detected via session.status: idle.
	return b.processSSE(ctx, cancel, sseResp.Body, srv, sessionID, opts)
}

// createSession creates a new OpenCode session via POST /session.
func (b *OpenCodeBackend) createSession(client *http.Client, srv *ocServer, opts RunOpts) (string, error) {
	body := map[string]any{}
	data, _ := json.Marshal(body)
	resp, err := client.Post(srv.baseURL+"/session", "application/json", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("POST /session returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode session response: %w", err)
	}

	sessionID, _ := result["id"].(string)
	if sessionID == "" {
		return "", fmt.Errorf("no session ID in response")
	}

	log.Printf("[opencode %s] created session %s", opts.InstanceID, sessionID)
	return sessionID, nil
}

// sendPrompt sends a prompt to an existing session via POST /session/:id/prompt_async.
func (b *OpenCodeBackend) sendPrompt(client *http.Client, srv *ocServer, sessionID string, opts RunOpts) error {
	body := map[string]any{
		"parts": []map[string]any{
			{"type": "text", "text": opts.Prompt},
		},
	}
	if opts.Model != "" {
		providerID, modelID := splitModel(opts.Model)
		body["model"] = map[string]string{
			"providerID": providerID,
			"modelID":    modelID,
		}
	}

	data, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/session/%s/prompt_async", srv.baseURL, sessionID)
	log.Printf("[opencode %s] POST %s", opts.InstanceID, url)
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST prompt_async returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// splitModel splits "provider/model" into provider and model parts.
func splitModel(model string) (providerID, modelID string) {
	idx := strings.IndexByte(model, '/')
	if idx < 0 {
		return "", model
	}
	return model[:idx], model[idx+1:]
}

// wrapAnswers converts mobile answer formats into OpenCode's expected format:
// [["choice1"], ["choice2"], ...] where each answer is an array of selected options.
func wrapAnswers(answers any) []any {
	switch v := answers.(type) {
	case []any:
		// Array of answers — wrap each element in an array if not already.
		result := make([]any, len(v))
		for i, item := range v {
			if arr, ok := item.([]any); ok {
				result[i] = arr
			} else {
				result[i] = []any{item}
			}
		}
		return result
	case map[string]any:
		// Object {question: answer} — extract values, each wrapped in an array.
		result := make([]any, 0, len(v))
		for _, val := range v {
			result = append(result, []any{val})
		}
		return result
	default:
		return []any{[]any{v}}
	}
}

// sseState tracks state across SSE events for a single session.
type sseState struct {
	// partTypes maps part ID → part type (e.g. "text", "tool", "reasoning").
	// Only parts with known type "text" on assistant messages have deltas emitted.
	partTypes map[string]string
	// messageRoles maps message ID → role ("user", "assistant").
	messageRoles map[string]string
	// seenBusy is true once we've seen the session enter busy/running state.
	// Used to avoid treating initial idle events as completion.
	seenBusy bool
	// completedAt is set when session idle is detected. We continue processing
	// SSE events briefly to drain any buffered events (text deltas, tool results).
	completedAt time.Time
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// processSSE reads the SSE stream and maps events to protocol messages.
func (b *OpenCodeBackend) processSSE(ctx context.Context, cancel context.CancelFunc, body io.Reader, srv *ocServer, sessionID string, opts RunOpts) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	state := &sseState{
		partTypes:    make(map[string]string),
		messageRoles: make(map[string]string),
	}
	var eventType string
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Empty line = end of event.
			if len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")

				// If no event: line was sent, try to extract the type from the JSON data.
				evtType := eventType
				if evtType == "" {
					var probe ocSSEEvent
					if json.Unmarshal([]byte(data), &probe) == nil && probe.Type != "" {
						evtType = probe.Type
					}
				}

				if evtType != "" {
					done, err := b.handleSSEEvent(ctx, evtType, data, srv, sessionID, opts, state)
					if err != nil {
						return err
					}
					if done && state.completedAt.IsZero() {
						// Mark completion time but continue draining events briefly.
						state.completedAt = time.Now()
					}
				}
				// If we've been draining for over 2 seconds after completion, stop.
				if !state.completedAt.IsZero() && time.Since(state.completedAt) > 2*time.Second {
					return nil
				}
			}
			eventType = ""
			dataLines = nil
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(line[6:])
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(line[5:]))
		}
		// Ignore id:, retry:, comments
	}

	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return nil // context cancelled, normal shutdown
		}
		return fmt.Errorf("SSE read error: %w", err)
	}
	return nil
}

// SSE event data structures from the OpenCode server.
// All events use: {"type": "...", "properties": {...}}

// ocSSEEvent is the generic envelope for all SSE events.
type ocSSEEvent struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// message.part.delta properties
type ocPartDelta struct {
	SessionID string `json:"sessionID"`
	PartID    string `json:"partID"`
	Field     string `json:"field"` // "text", etc.
	Delta     string `json:"delta"`
}

// message.part.updated / message.part.created properties
type ocPartPayload struct {
	Part ocPart `json:"part"`
}

type ocPart struct {
	ID        string       `json:"id"`
	SessionID string       `json:"sessionID"`
	Type      string       `json:"type"` // "text", "tool"
	MessageID string       `json:"messageID"`
	Text      string       `json:"text,omitempty"`
	CallID    string       `json:"callID,omitempty"`
	Tool      string       `json:"tool,omitempty"`
	State     *ocToolState `json:"state,omitempty"`
}

type ocToolState struct {
	Status string          `json:"status"` // "pending", "running", "completed", "error"
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
}

// permission.asked properties
type ocPermissionAsked struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionID"`
	Permission string          `json:"permission"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// question.asked properties
type ocQuestionAsked struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionID"`
	Questions json.RawMessage `json:"questions"` // array of question objects
}

// session.updated properties
type ocSessionUpdated struct {
	Info struct {
		ID string `json:"id"`
	} `json:"info"`
}

// session.idle properties
type ocSessionIdle struct {
	SessionID string `json:"sessionID"`
}

// session.status properties
type ocSessionStatus struct {
	SessionID string `json:"sessionID"`
	Status    struct {
		Type string `json:"type"` // "idle", "busy", "retry"
	} `json:"status"`
}

// handleSSEEvent processes a single SSE event. Returns (done, error) where done=true
// means the session is complete and we should stop processing.
func (b *OpenCodeBackend) handleSSEEvent(ctx context.Context, eventType, data string, srv *ocServer, sessionID string, opts RunOpts, state *sseState) (bool, error) {
	// Parse the generic event envelope to extract properties.
	var envelope ocSSEEvent
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		log.Printf("[opencode %s] failed to parse SSE event envelope: %v", opts.InstanceID, err)
		return false, nil
	}
	props := envelope.Properties

	switch eventType {
	case "message.updated":
		// Track message roles so we can filter user message parts.
		var msgUpdate struct {
			Info struct {
				ID   string `json:"id"`
				Role string `json:"role"`
			} `json:"info"`
		}
		if json.Unmarshal(props, &msgUpdate) == nil && msgUpdate.Info.ID != "" {
			state.messageRoles[msgUpdate.Info.ID] = msgUpdate.Info.Role
		}

	case "message.part.delta":
		var delta ocPartDelta
		if err := json.Unmarshal(props, &delta); err != nil {
			return false, nil
		}
		if delta.SessionID != sessionID {
			return false, nil
		}
		state.seenBusy = true
		// Only emit deltas for parts registered as assistant "text" type.
		// Filters out: reasoning parts, user message parts, unknown parts.
		if state.partTypes[delta.PartID] != "text" {
			return false, nil
		}
		if delta.Delta != "" {
			opts.Emitter(protocol.TypeStreamText, protocol.StreamText{
				InstanceID: opts.InstanceID,
				Text:       delta.Delta,
				IsPartial:  true,
			})
		}

	case "message.part.created", "message.part.updated":
		var payload ocPartPayload
		if err := json.Unmarshal(props, &payload); err != nil {
			return false, nil
		}
		if payload.Part.SessionID != sessionID {
			return false, nil
		}
		state.seenBusy = true

		// Skip parts belonging to user messages (prevents prompt echo).
		if state.messageRoles[payload.Part.MessageID] == "user" {
			state.partTypes[payload.Part.ID] = "user"
			return false, nil
		}

		isNew := state.partTypes[payload.Part.ID] == ""

		// Record assistant part types so deltas can be filtered.
		// Only register "text" and "tool" — everything else (reasoning, etc.) stays unregistered.
		switch payload.Part.Type {
		case "text":
			state.partTypes[payload.Part.ID] = "text"
			// Don't emit full text here — deltas handle streaming.
		case "tool":
			state.partTypes[payload.Part.ID] = "tool"
			if isNew {
				// First time seeing this tool — emit tool use event.
				b.emitToolStart(opts, payload.Part)
			}
			// Check for tool completion on every update.
			if payload.Part.State != nil && (payload.Part.State.Status == "completed" || payload.Part.State.Status == "error") {
				b.emitToolResult(opts, payload.Part)
			}
		}

	case "permission.asked":
		var perm ocPermissionAsked
		if err := json.Unmarshal(props, &perm); err != nil {
			log.Printf("[opencode %s] failed to parse permission.asked: %v", opts.InstanceID, err)
			return false, nil
		}
		if perm.SessionID != sessionID {
			return false, nil
		}

		log.Printf("[opencode %s] permission.asked: id=%s permission=%s", opts.InstanceID, perm.ID, perm.Permission)
		opts.Emitter(protocol.TypePermissionRequest, protocol.PermissionRequestMsg{
			InstanceID: opts.InstanceID,
			RequestID:  perm.ID,
			ToolName:   perm.Permission,
			ToolInput:  perm.Metadata,
		})

		if opts.PermissionResolver != nil {
			go func() {
				resp, err := opts.PermissionResolver.WaitForResponse(ctx, perm.ID)
				if err != nil {
					log.Printf("[opencode %s] permission wait error: %v", opts.InstanceID, err)
					return
				}
				b.respondPermission(srv, sessionID, perm.ID, resp, opts.InstanceID)
			}()
		}

	case "question.asked":
		var q ocQuestionAsked
		if err := json.Unmarshal(props, &q); err != nil {
			log.Printf("[opencode %s] failed to parse question.asked: %v", opts.InstanceID, err)
			return false, nil
		}
		if q.SessionID != sessionID {
			return false, nil
		}
		if q.ID == "" {
			log.Printf("[opencode %s] question.asked missing ID", opts.InstanceID)
			return false, nil
		}

		log.Printf("[opencode %s] question.asked: id=%s", opts.InstanceID, q.ID)
		opts.Emitter(protocol.TypeAskUserQuestion, protocol.AskUserQuestion{
			InstanceID: opts.InstanceID,
			RequestID:  q.ID,
			Questions:  q.Questions,
		})

		if opts.PermissionResolver != nil {
			go func() {
				resp, err := opts.PermissionResolver.WaitForResponse(ctx, q.ID)
				if err != nil {
					log.Printf("[opencode %s] question wait error: %v", opts.InstanceID, err)
					return
				}
				b.respondQuestion(srv, q.ID, resp, opts.InstanceID)
			}()
		}

	case "session.idle":
		log.Printf("[opencode %s] session.idle (seenBusy=%v)", opts.InstanceID, state.seenBusy)
		if state.seenBusy && state.completedAt.IsZero() {
			opts.Emitter(protocol.TypeStreamComplete, protocol.StreamComplete{
				InstanceID: opts.InstanceID,
			})
			return true, nil // signals completion, processSSE starts drain timer
		}

	case "session.status":
		var status ocSessionStatus
		if err := json.Unmarshal(props, &status); err == nil {
			if status.Status.Type == "busy" || status.Status.Type == "running" {
				state.seenBusy = true
			} else if status.Status.Type == "idle" && state.seenBusy && state.completedAt.IsZero() {
				log.Printf("[opencode %s] session idle via session.status, completing", opts.InstanceID)
				opts.Emitter(protocol.TypeStreamComplete, protocol.StreamComplete{
					InstanceID: opts.InstanceID,
				})
				return true, nil // signals completion, processSSE starts drain timer
			}
		}

	case "session.updated":
		log.Printf("[opencode %s] session.updated props: %s", opts.InstanceID, truncate(string(props), 500))
		// Check if session.updated carries idle status (fallback completion detection).
		var raw map[string]json.RawMessage
		if json.Unmarshal(props, &raw) == nil {
			// Try properties.info.status or properties.status
			if infoRaw, ok := raw["info"]; ok {
				var info struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				}
				if json.Unmarshal(infoRaw, &info) == nil {
					if info.Status == "running" || info.Status == "busy" {
						state.seenBusy = true
					} else if info.Status == "idle" && state.seenBusy && state.completedAt.IsZero() {
						log.Printf("[opencode %s] session.updated idle, completing", opts.InstanceID)
						opts.Emitter(protocol.TypeStreamComplete, protocol.StreamComplete{
							InstanceID: opts.InstanceID,
						})
						return true, nil
					}
				}
			}
		}

	case "session.error":
		log.Printf("[opencode %s] session.error props: %s", opts.InstanceID, string(props))
		opts.Emitter(protocol.TypeNotification, protocol.Notification{
			InstanceID: opts.InstanceID,
			Type:       "error",
			Message:    "OpenCode session encountered an error",
		})

	default:
		// Suppress noisy events from logging.
		switch eventType {
		case "server.heartbeat", "server.connected",
			"session.diff", "file.edited", "file.watcher.updated",
			"lsp.updated", "lsp.client.diagnostics", "question.replied":
			// Ignore silently.
		default:
			log.Printf("[opencode %s] unhandled SSE event %q: %s", opts.InstanceID, eventType, truncate(data, 300))
		}
	}

	return false, nil
}

// emitToolStart emits a tool use start event (called once when tool part is first seen).
func (b *OpenCodeBackend) emitToolStart(opts RunOpts, part ocPart) {
	input := json.RawMessage(`{}`)
	if part.State != nil && part.State.Input != nil {
		input = part.State.Input
	}
	opts.Emitter(protocol.TypeStreamToolUse, protocol.StreamToolUse{
		InstanceID: opts.InstanceID,
		ToolName:   part.Tool,
		ToolInput:  input,
		ToolUseID:  part.CallID,
	})
}

// emitToolResult emits a tool result event (called when tool state is completed/error).
func (b *OpenCodeBackend) emitToolResult(opts RunOpts, part ocPart) {
	output := "(no output)"
	if part.State != nil && part.State.Output != "" {
		output = part.State.Output
	}
	opts.Emitter(protocol.TypeStreamToolResult, protocol.StreamToolResult{
		InstanceID: opts.InstanceID,
		ToolUseID:  part.CallID,
		Output:     output,
		IsError:    part.State != nil && part.State.Status == "error",
	})
}

// mapDecisionToResponse maps the mobile app's "allow"/"deny" to OpenCode's permission response format.
func mapDecisionToResponse(decision string) string {
	switch decision {
	case "allow":
		return "once"
	case "always":
		return "always"
	case "deny", "reject":
		return "reject"
	default:
		return "once"
	}
}

// respondPermission forwards a permission response to the OpenCode server.
// OpenCode expects: POST /session/{sessionID}/permissions/{permID} with {"response": "once"|"always"|"reject"}
func (b *OpenCodeBackend) respondPermission(srv *ocServer, sessionID, permID string, response map[string]any, instanceID string) {
	decision, _ := response["decision"].(string)
	ocResponse := map[string]string{
		"response": mapDecisionToResponse(decision),
	}
	data, _ := json.Marshal(ocResponse)
	url := fmt.Sprintf("%s/session/%s/permissions/%s", srv.baseURL, sessionID, permID)
	log.Printf("[opencode %s] POST %s (decision=%s → response=%s)", instanceID, url, decision, ocResponse["response"])

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("[opencode %s] failed to respond to permission %s: %v", instanceID, permID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[opencode %s] permission response returned %d: %s", instanceID, resp.StatusCode, string(body))
	}
}

// respondQuestion forwards a question answer to the OpenCode server.
// OpenCode expects: POST /question/{questionID}/reply with {"answers": ["answer1", "answer2", ...]}
func (b *OpenCodeBackend) respondQuestion(srv *ocServer, questionID string, response map[string]any, instanceID string) {
	// OpenCode expects {"answers": [["choice1a", "choice1b"], ["choice2a"], ...]}
	// Each answer is an array of selected options (multi-select support).
	// The mobile app sends {"answers": {"question": "answer", ...}} or {"answers": ["a", "b"]}.
	body := map[string]any{}

	if answers, ok := response["answers"]; ok {
		body["answers"] = wrapAnswers(answers)
	} else if answer, ok := response["answer"]; ok {
		body["answers"] = []any{[]any{answer}}
	} else {
		arr := make([]any, 0)
		for k, v := range response {
			if k == "request_id" || k == "requestID" {
				continue
			}
			arr = append(arr, []any{v})
		}
		body["answers"] = arr
	}

	data, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/question/%s/reply", srv.baseURL, questionID)
	log.Printf("[opencode %s] POST %s", instanceID, url)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("[opencode %s] failed to respond to question %s: %v", instanceID, questionID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("[opencode %s] question response returned %d: %s", instanceID, resp.StatusCode, string(respBody))
	}
}

// findFreePort returns a free TCP port on localhost.
func findFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// --- Provider discovery from OpenCode models cache ---

// ocCachedProvider is a provider entry in ~/.cache/opencode/models.json.
type ocCachedProvider struct {
	ID     string                   `json:"id"`
	Name   string                   `json:"name"`
	Env    []string                 `json:"env"`
	Models map[string]ocCachedModel `json:"models"`
}

type ocCachedModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func discoverOpenCodeProviders() []ProviderInfo {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	// Read the auth file to find which providers are configured.
	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	authData, err := os.ReadFile(authPath)
	if err != nil {
		return nil
	}
	var auth map[string]json.RawMessage
	if err := json.Unmarshal(authData, &auth); err != nil {
		log.Printf("[opencode] failed to parse auth.json: %v", err)
		return nil
	}

	// Read the models cache to get model lists per provider.
	cachePath := filepath.Join(home, ".cache", "opencode", "models.json")
	cacheData, err := os.ReadFile(cachePath)
	if err != nil {
		return nil
	}
	var cache map[string]ocCachedProvider
	if err := json.Unmarshal(cacheData, &cache); err != nil {
		log.Printf("[opencode] failed to parse models cache: %v", err)
		return nil
	}

	// Only include providers the user has authenticated with.
	var providers []ProviderInfo
	for id := range auth {
		p, ok := cache[id]
		if !ok {
			continue
		}
		name := p.Name
		if name == "" {
			name = id
		}
		models := make([]string, 0, len(p.Models))
		for modelID := range p.Models {
			models = append(models, modelID)
		}
		if len(models) == 0 {
			continue
		}
		providers = append(providers, ProviderInfo{
			ID:     id,
			Name:   name,
			Models: models,
		})
	}
	return providers
}
