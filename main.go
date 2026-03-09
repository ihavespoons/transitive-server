package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ihavespoons/transitive/internal/backend"
	"github.com/ihavespoons/transitive/internal/bgprocess"
	"github.com/ihavespoons/transitive/internal/config"
	"github.com/ihavespoons/transitive/internal/hooks"
	"github.com/ihavespoons/transitive/internal/instance"
	"github.com/ihavespoons/transitive/internal/pairing"
	"github.com/ihavespoons/transitive/internal/portforward"
	"github.com/ihavespoons/transitive/internal/protocol"
	"github.com/ihavespoons/transitive/internal/relay"
	"github.com/ihavespoons/transitive/internal/shell"
	"github.com/google/uuid"
)

var version = "dev"

func main() {
	// Handle subcommands before flag parsing.
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		runHook()
		return
	}

	cfg := config.DefaultConfig()
	if err := cfg.LoadFromFile(); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.RelayURL, "relay-url", cfg.RelayURL, "Cloudflare relay WebSocket URL")
	flag.BoolVar(&cfg.NoRelay, "no-relay", false, "Run without relay connection (local only)")
	flag.StringVar(&cfg.ClaudePath, "claude-path", cfg.ClaudePath, "Path to claude CLI")
	flag.StringVar(&cfg.OpenCodePath, "opencode-path", cfg.OpenCodePath, "Path to opencode CLI")
	flag.StringVar(&cfg.ProjectDir, "project-dir", cfg.ProjectDir, "Base directory for cloned repositories")
	flag.BoolVar(&cfg.EnableClaude, "enable-claude", cfg.EnableClaude, "Enable Claude Code backend (experimental)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	installHooks := flag.Bool("install-hooks", false, "Install hooks and exit")
	uninstallHooks := flag.Bool("uninstall-hooks", false, "Uninstall hooks and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	// Handle hook install/uninstall commands.
	if *uninstallHooks {
		if err := hooks.Uninstall(); err != nil {
			log.Fatalf("failed to uninstall hooks: %v", err)
		}
		if err := hooks.UninstallOpenCode(); err != nil {
			log.Printf("warning: failed to uninstall OpenCode plugin: %v", err)
		}
		fmt.Println("Hooks uninstalled.")
		return
	}

	if *installHooks {
		exePath, _ := os.Executable()
		if err := hooks.Install(exePath, cfg.Port); err != nil {
			log.Fatalf("failed to install hooks: %v", err)
		}
		fmt.Println("Hooks installed.")
		return
	}

	// Load or generate agent credentials.
	if err := loadOrCreateCredentials(cfg); err != nil {
		log.Fatalf("failed to load credentials: %v", err)
	}

	// Install hooks using the current binary.
	exePath, _ := os.Executable()
	if cfg.EnableClaude {
		if err := hooks.Install(exePath, cfg.Port); err != nil {
			log.Printf("warning: failed to install hooks: %v", err)
		}
	}
	if err := hooks.InstallOpenCode(cfg.Port); err != nil {
		log.Printf("warning: failed to install OpenCode plugin: %v", err)
	}

	// Create backend registry and register backends.
	reg := backend.NewRegistry()
	if cfg.EnableClaude {
		reg.Register(backend.NewClaudeBackend(cfg.ClaudePath))
	}
	reg.Register(backend.NewOpenCodeBackend(cfg.OpenCodePath))
	reg.RefreshAll()

	// Set up the relay client (if enabled).
	var relayClient *relay.Client
	var hookHandler *hooks.Handler
	var shellMgr *shell.Manager
	var pfMgr *portforward.Manager
	var bgprocessMgr *bgprocess.Manager

	// Permission router: maps requestID → instanceID for OpenCode server API responses.
	permRouter := &permissionRouter{requests: make(map[string]string)}

	// The event sink sends messages from instances/hooks → relay → mobile.
	var eventSink instance.EventSink

	// Instance manager.
	mgr := instance.NewManager(nil, reg, cfg.ProjectDir) // sink set below

	if !cfg.NoRelay {
		relayClient = relay.NewClient(cfg.RelayURL, cfg.AgentID, cfg.Secret, func(env *protocol.Envelope) {
			// Messages from mobile → server.
			switch env.Type {
			case protocol.TypePermissionRespond:
				var resp protocol.PermissionResponse
				if err := json.Unmarshal(env.Payload, &resp); err != nil {
					log.Printf("invalid permission.respond: %v", err)
					return
				}
				response := map[string]any{"decision": resp.Decision}
				// Try OpenCode API path first, fall back to hooks.
				if instanceID := permRouter.Lookup(resp.RequestID); instanceID != "" {
					if inst := mgr.Get(instanceID); inst != nil {
						if managed, ok := inst.(*instance.ManagedInstance); ok {
							managed.ResolvePermission(resp.RequestID, response)
							return
						}
					}
				}
				if hookHandler != nil {
					hookHandler.ResolvePermission(resp.RequestID, response)
				}

			case protocol.TypeAskUserAnswer:
				var raw map[string]any
				if err := json.Unmarshal(env.Payload, &raw); err != nil {
					log.Printf("invalid ask.user.answer: %v", err)
					return
				}
				requestID, _ := raw["request_id"].(string)
				if requestID == "" {
					return
				}
				// Try OpenCode API path first, fall back to hooks.
				if instanceID := permRouter.Lookup(requestID); instanceID != "" {
					if inst := mgr.Get(instanceID); inst != nil {
						if managed, ok := inst.(*instance.ManagedInstance); ok {
							managed.ResolvePermission(requestID, raw)
							return
						}
					}
				}
				if hookHandler != nil {
					hookHandler.ResolvePermission(requestID, raw)
				}

			case protocol.TypeShellStart:
				var req protocol.ShellStart
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid shell.start: %v", err)
					return
				}
				cwd := ""
				if inst := mgr.Get(req.InstanceID); inst != nil {
					cwd = inst.Cwd()
				}
				if cwd == "" {
					cwd = "/"
				}
				if err := shellMgr.Start(req.InstanceID, cwd, req.Cols, req.Rows); err != nil {
					log.Printf("shell.start error: %v", err)
				}

			case protocol.TypeShellInput:
				var req protocol.ShellInput
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid shell.input: %v", err)
					return
				}
				if err := shellMgr.Input(req.InstanceID, req.Data); err != nil {
					log.Printf("shell.input error: %v", err)
				}

			case protocol.TypeShellResize:
				var req protocol.ShellResize
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid shell.resize: %v", err)
					return
				}
				shellMgr.Resize(req.InstanceID, req.Cols, req.Rows)

			case protocol.TypeShellStop:
				var req protocol.ShellStopRequest
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid shell.stop: %v", err)
					return
				}
				shellMgr.Stop(req.InstanceID)

			case protocol.TypePortForwardStart:
				var req protocol.PortForwardStart
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid port_forward.start: %v", err)
					return
				}
				if err := pfMgr.Start(req.InstanceID, req.Port); err != nil {
					log.Printf("port_forward.start error: %v", err)
				}

			case protocol.TypePortForwardStop:
				var req protocol.PortForwardStopRequest
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid port_forward.stop: %v", err)
					return
				}
				pfMgr.Stop(req.InstanceID, req.Port)

			case protocol.TypeProxyRequest:
				var req protocol.ProxyRequest
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid proxy.request: %v", err)
					return
				}
				go pfMgr.HandleProxyRequest(req)

			case protocol.TypeBgProcessStart:
				var req protocol.BackgroundProcessStartRequest
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid background.process.start: %v", err)
					return
				}
				cwd := ""
				if inst := mgr.Get(req.InstanceID); inst != nil {
					cwd = inst.Cwd()
				}
				if _, err := bgprocessMgr.Start(req.InstanceID, req.Name, req.Command, cwd, req.Port); err != nil {
					log.Printf("background.process.start error: %v", err)
				} else if req.Port > 0 {
					if err := pfMgr.Start(req.InstanceID, req.Port); err != nil {
						log.Printf("auto port-forward for port %d failed: %v", req.Port, err)
					}
				}

			case protocol.TypeBgProcessStop:
				var req protocol.BackgroundProcessStopRequest
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid background.process.stop: %v", err)
					return
				}
				if err := bgprocessMgr.Stop(req.ProcessID); err != nil {
					log.Printf("background.process.stop error: %v", err)
				}

			case protocol.TypeBgProcessRestart:
				var req protocol.BackgroundProcessRestartRequest
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid background.process.restart: %v", err)
					return
				}
				if _, err := bgprocessMgr.Restart(req.ProcessID); err != nil {
					log.Printf("background.process.restart error: %v", err)
				}

			case protocol.TypeBgProcessListRequest:
				var req protocol.BackgroundProcessListRequest
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid background.process.list.request: %v", err)
					return
				}
				procs := bgprocessMgr.List(req.InstanceID)
				env, err := protocol.NewEnvelope(protocol.TypeBgProcessList, uuid.New().String(), protocol.BackgroundProcessList{
					InstanceID: req.InstanceID,
					Processes:  procs,
				})
				if err == nil {
					eventSink(env)
				}

			case protocol.TypePortForwardListRequest:
				var req protocol.PortForwardListRequest
				if err := json.Unmarshal(env.Payload, &req); err != nil {
					log.Printf("invalid port_forward.list.request: %v", err)
					return
				}
				fwds := pfMgr.List(req.InstanceID)
				env, err := protocol.NewEnvelope(protocol.TypePortForwardList, uuid.New().String(), protocol.PortForwardList{
					InstanceID: req.InstanceID,
					Forwards:   fwds,
				})
				if err == nil {
					eventSink(env)
				}

			default:
				// Clean up shell/port forwards when an instance is stopped or removed.
				if env.Type == protocol.TypeInstanceStop || env.Type == protocol.TypeInstanceRemove {
					var req protocol.InstanceStopRequest
					if err := json.Unmarshal(env.Payload, &req); err == nil {
						shellMgr.Stop(req.InstanceID)
						pfMgr.StopAllForInstance(req.InstanceID)
						bgprocessMgr.StopAllForInstance(req.InstanceID)
					}
				}
				mgr.HandleMessage(env)
			}
		})

		eventSink = func(env *protocol.Envelope) {
			if err := relayClient.Send(env); err != nil {
				log.Printf("failed to send to relay: %v", err)
			}
		}
	} else {
		eventSink = func(env *protocol.Envelope) {
			data, _ := json.Marshal(env)
			log.Printf("[event] %s", string(data))
		}
	}

	// Wire the event sink into the manager.
	mgr = instance.NewManager(eventSink, reg, cfg.ProjectDir)
	mgr.SetOnPermEmit(permRouter.Register)
	mgr.LoadPersisted()

	// Background process, shell, and port forward managers.
	bgprocessMgr = bgprocess.NewManager(bgprocess.EventSink(eventSink))
	shellMgr = shell.NewManager(shell.EventSink(eventSink))

	// Derive relay host from relay URL for proxy URLs.
	relayHost := "relay.transitive.dev"
	if parsed, err := url.Parse(cfg.RelayURL); err == nil && parsed.Host != "" {
		relayHost = parsed.Host
	}
	pfMgr = portforward.NewManager(portforward.EventSink(eventSink), relayHost, cfg.AgentID)

	// Auto-cleanup port forwards when background processes stop.
	bgprocessMgr.OnStopped = func(instanceID string, port int) {
		pfMgr.Stop(instanceID, port)
	}

	// Hook handler.
	hookHandler = hooks.NewHandler(mgr, eventSink, bgprocessMgr, pfMgr)

	// HTTP server for hooks.
	mux := http.NewServeMux()
	mux.Handle("/hook", hookHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:    cfg.ListenAddr(),
		Handler: mux,
	}

	// Connect to relay.
	if relayClient != nil {
		if err := relayClient.Connect(); err != nil {
			log.Fatalf("failed to connect to relay: %v", err)
		}
		defer relayClient.Close()

		// Display QR code.
		if err := pairing.PrintQR(cfg.RelayURL, cfg.AgentID, cfg.Secret); err != nil {
			log.Printf("failed to display QR code: %v", err)
		}
	}

	// Start HTTP server.
	go func() {
		log.Printf("HTTP server listening on %s", cfg.ListenAddr())
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Periodic backend provider refresh (every 60 seconds).
	refreshTicker := time.NewTicker(60 * time.Second)
	go func() {
		for range refreshTicker.C {
			reg.RefreshAll()
		}
	}()

	if cfg.NoRelay {
		fmt.Println("Running in local-only mode (no relay).")
		fmt.Printf("Hook endpoint: http://localhost:%d/hook\n", cfg.Port)
	}

	// Wait for interrupt.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	refreshTicker.Stop()
	log.Println("shutting down...")
	bgprocessMgr.StopAll()
	shellMgr.StopAll()
	server.Close()
}

type savedCredentials struct {
	AgentID string `json:"agent_id"`
	Secret  string `json:"secret"`
}

func credentialsPath() string {
	dir := filepath.Join(os.TempDir(), "transitive")
	if u, err := user.Current(); err == nil {
		dir = filepath.Join(u.HomeDir, ".transitive")
	}
	return filepath.Join(dir, "credentials.json")
}

func loadOrCreateCredentials(cfg *config.Config) error {
	path := credentialsPath()

	data, err := os.ReadFile(path)
	if err == nil {
		var creds savedCredentials
		if json.Unmarshal(data, &creds) == nil && creds.AgentID != "" && creds.Secret != "" {
			cfg.AgentID = creds.AgentID
			cfg.Secret = creds.Secret
			log.Printf("loaded credentials from %s", path)
			return nil
		}
	}

	// Generate new credentials and save them.
	cfg.AgentID = uuid.New().String()
	cfg.Secret = uuid.New().String()

	creds := savedCredentials{AgentID: cfg.AgentID, Secret: cfg.Secret}
	data, err = json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}

	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Printf("warning: could not save credentials: %v", err)
	} else {
		log.Printf("saved new credentials to %s", path)
	}
	return nil
}

// permissionRouter maps requestID → instanceID for routing permission/question
// responses to the correct OpenCode managed instance.
type permissionRouter struct {
	mu       sync.Mutex
	requests map[string]string
}

func (r *permissionRouter) Register(requestID, instanceID string) {
	r.mu.Lock()
	r.requests[requestID] = instanceID
	r.mu.Unlock()
}

func (r *permissionRouter) Lookup(requestID string) string {
	r.mu.Lock()
	instanceID, ok := r.requests[requestID]
	if ok {
		delete(r.requests, requestID) // one-shot
	}
	r.mu.Unlock()
	return instanceID
}

func runHook() {
	debugFile, _ := os.OpenFile("/tmp/transitive-hook-debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)

	port := 7865
	if p := os.Getenv("TRANSITIVE_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	body, readErr := io.ReadAll(os.Stdin)
	if debugFile != nil {
		// Extract hook_event_name and tool_name for quick identification.
		var probe struct {
			Event    string `json:"hook_event_name"`
			ToolName string `json:"tool_name"`
		}
		json.Unmarshal(body, &probe)
		fmt.Fprintf(debugFile, "[%s] event=%s tool=%s stdin=%d bytes, readErr=%v\n", time.Now().Format(time.RFC3339Nano), probe.Event, probe.ToolName, len(body), readErr)
	}
	hookURL := fmt.Sprintf("http://localhost:%d/hook", port)
	client := &http.Client{Timeout: 300 * time.Second}
	req, err := http.NewRequest("POST", hookURL, bytes.NewReader(body))
	if err != nil {
		if debugFile != nil {
			fmt.Fprintf(debugFile, "[%s] NewRequest error: %v\n", time.Now().Format(time.RFC3339Nano), err)
		}
		fmt.Print(`{}`)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Send parent PID so the server can signal the Claude CLI process.
	// With "VAR=val cmd" syntax, sh typically exec's the command directly,
	// so PPID is the Claude CLI process.
	req.Header.Set("X-Claude-PID", fmt.Sprintf("%d", os.Getppid()))
	resp, err := client.Do(req)
	if err != nil {
		if debugFile != nil {
			fmt.Fprintf(debugFile, "[%s] HTTP error: %v\n", time.Now().Format(time.RFC3339Nano), err)
		}
		// Server unreachable — auto-allow so the CLI isn't blocked.
		fmt.Print(`{}`)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if debugFile != nil {
		fmt.Fprintf(debugFile, "[%s] HTTP %d, response=%s\n", time.Now().Format(time.RFC3339Nano), resp.StatusCode, string(respBody[:min(len(respBody), 200)]))
		debugFile.Close()
	}
	os.Stdout.Write(respBody)
}
