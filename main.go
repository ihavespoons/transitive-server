package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"syscall"

	"github.com/ihavespoons/claudette-server/internal/config"
	"github.com/ihavespoons/claudette-server/internal/hooks"
	"github.com/ihavespoons/claudette-server/internal/instance"
	"github.com/ihavespoons/claudette-server/internal/pairing"
	"github.com/ihavespoons/claudette-server/internal/protocol"
	"github.com/ihavespoons/claudette-server/internal/relay"
	"github.com/google/uuid"
)

var version = "dev"

//go:embed scripts/claudette-hook.sh
var hookScript string

func main() {
	cfg := config.DefaultConfig()

	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.RelayURL, "relay-url", cfg.RelayURL, "Cloudflare relay WebSocket URL")
	flag.BoolVar(&cfg.NoRelay, "no-relay", false, "Run without relay connection (local only)")
	flag.StringVar(&cfg.ClaudePath, "claude-path", cfg.ClaudePath, "Path to claude CLI")
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
		fmt.Println("Hooks uninstalled.")
		return
	}

	if *installHooks {
		hookPath := writeHookScript()
		if err := hooks.Install(hookPath, cfg.Port); err != nil {
			log.Fatalf("failed to install hooks: %v", err)
		}
		fmt.Println("Hooks installed.")
		return
	}

	// Load or generate agent credentials.
	if err := loadOrCreateCredentials(cfg); err != nil {
		log.Fatalf("failed to load credentials: %v", err)
	}

	// Write hook script and install hooks.
	hookPath := writeHookScript()
	if err := hooks.Install(hookPath, cfg.Port); err != nil {
		log.Printf("warning: failed to install hooks: %v", err)
	}

	// Set up the relay client (if enabled).
	var relayClient *relay.Client
	var hookHandler *hooks.Handler

	// The event sink sends messages from instances/hooks → relay → mobile.
	var eventSink instance.EventSink

	// Instance manager.
	mgr := instance.NewManager(nil) // sink set below

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
				if hookHandler != nil {
					hookHandler.ResolvePermission(resp.RequestID, resp.Decision)
				}
			default:
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
	mgr = instance.NewManager(eventSink)

	// Hook handler.
	hookHandler = hooks.NewHandler(mgr, eventSink)

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

	if cfg.NoRelay {
		fmt.Println("Running in local-only mode (no relay).")
		fmt.Printf("Hook endpoint: http://localhost:%d/hook\n", cfg.Port)
	}

	// Wait for interrupt.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("shutting down...")
	server.Close()
}

type savedCredentials struct {
	AgentID string `json:"agent_id"`
	Secret  string `json:"secret"`
}

func credentialsPath() string {
	dir := filepath.Join(os.TempDir(), "claudette")
	if u, err := user.Current(); err == nil {
		dir = filepath.Join(u.HomeDir, ".claudette")
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

func writeHookScript() string {
	dir := filepath.Join(os.TempDir(), "claudette")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "claudette-hook.sh")
	os.WriteFile(path, []byte(hookScript), 0o755)
	return path
}
