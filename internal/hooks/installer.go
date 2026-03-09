package hooks

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// The hook events we install handlers for.
var hookEvents = []string{
	"PreToolUse",
	"PostToolUse",
	"Notification",
	"Stop",
}

// hookEntry represents a single hook command inside a matcher group.
type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// hookGroup represents the new matcher-based hook format.
type hookGroup struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

// Install writes hook configuration to ~/.claude/settings.json.
func Install(binaryPath string, port int) error {
	settingsPath := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")

	// Read existing settings or start fresh.
	settings := make(map[string]any)
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse settings: %w", err)
		}
	}

	// Build hooks config.
	hooks := make(map[string]any)
	if existing, ok := settings["hooks"]; ok {
		if m, ok := existing.(map[string]any); ok {
			hooks = m
		}
	}

	command := fmt.Sprintf("TRANSITIVE_PORT=%d %s hook", port, binaryPath)

	newGroup := hookGroup{
		Matcher: "",
		Hooks: []hookEntry{{
			Type:    "command",
			Command: command,
			Timeout: 300,
		}},
	}

	for _, event := range hookEvents {
		var eventHooks []any
		if existing, ok := hooks[event]; ok {
			if arr, ok := existing.([]any); ok {
				alreadyInstalled := false
				for i, h := range arr {
					if isTransitiveEntry(h) {
						// Replace with new format.
						arr[i] = newGroup
						alreadyInstalled = true
						break
					}
				}
				if alreadyInstalled {
					eventHooks = arr
				} else {
					eventHooks = append(arr, newGroup)
				}
			}
		}
		if eventHooks == nil {
			eventHooks = []any{newGroup}
		}
		hooks[event] = eventHooks
	}

	settings["hooks"] = hooks

	// Write back.
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}

	if err := os.WriteFile(settingsPath, out, 0o644); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}

	log.Printf("hooks installed in %s", settingsPath)
	return nil
}

// Uninstall removes transitive hooks from ~/.claude/settings.json.
func Uninstall() error {
	settingsPath := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil // Nothing to uninstall.
	}

	settings := make(map[string]any)
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("parse settings: %w", err)
	}

	hooks, ok := settings["hooks"]
	if !ok {
		return nil
	}

	hooksMap, ok := hooks.(map[string]any)
	if !ok {
		return nil
	}

	for _, event := range hookEvents {
		existing, ok := hooksMap[event]
		if !ok {
			continue
		}
		arr, ok := existing.([]any)
		if !ok {
			continue
		}

		var filtered []any
		for _, h := range arr {
			if isTransitiveEntry(h) {
				continue
			}
			filtered = append(filtered, h)
		}

		if len(filtered) == 0 {
			delete(hooksMap, event)
		} else {
			hooksMap[event] = filtered
		}
	}

	if len(hooksMap) == 0 {
		delete(settings, "hooks")
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}

	return os.WriteFile(settingsPath, out, 0o644)
}

// isTransitiveEntry checks if a hook entry (old or new format) is a transitive hook.
func isTransitiveEntry(h any) bool {
	m, ok := h.(map[string]any)
	if !ok {
		return false
	}

	// Old format: {"type": "command", "command": "TRANSITIVE_PORT=..."}
	if cmd, ok := m["command"].(string); ok {
		return isTransitiveCommand(cmd)
	}

	// New format: {"matcher": "", "hooks": [{"command": "TRANSITIVE_PORT=..."}]}
	if hooksArr, ok := m["hooks"].([]any); ok {
		for _, inner := range hooksArr {
			if im, ok := inner.(map[string]any); ok {
				if cmd, ok := im["command"].(string); ok && isTransitiveCommand(cmd) {
					return true
				}
			}
		}
	}

	return false
}

func isTransitiveCommand(cmd string) bool {
	return len(cmd) > 0 && (contains(cmd, "transitive-hook") || contains(cmd, "TRANSITIVE_PORT") || contains(cmd, "transitive hook") ||
		contains(cmd, "claudette-hook") || contains(cmd, "CLAUDETTE_PORT") || contains(cmd, "claudette hook"))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
