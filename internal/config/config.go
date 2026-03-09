package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
)

type Config struct {
	Port         int    `json:"port,omitempty"`
	RelayURL     string `json:"relay_url,omitempty"`
	NoRelay      bool   `json:"no_relay,omitempty"`
	AgentID      string `json:"-"`
	Secret       string `json:"-"`
	HookPort     int    `json:"hook_port,omitempty"`
	ClaudePath   string `json:"claude_path,omitempty"`
	OpenCodePath string `json:"opencode_path,omitempty"`
	ProjectDir   string `json:"project_dir,omitempty"`
	EnableClaude bool   `json:"enable_claude,omitempty"`
}

func (c *Config) HookAddr() string {
	return fmt.Sprintf("localhost:%d", c.HookPort)
}

func (c *Config) ListenAddr() string {
	return fmt.Sprintf(":%d", c.Port)
}

func DefaultConfig() *Config {
	return &Config{
		Port:         7865,
		RelayURL:     "wss://relay.transitive.dev",
		HookPort:     7865,
		ClaudePath:   "claude",
		OpenCodePath: "opencode",
		ProjectDir:   "~/Code",
	}
}

// ConfigPath returns the path to the config file (~/.transitive/config.json).
func ConfigPath() string {
	if u, err := user.Current(); err == nil {
		return filepath.Join(u.HomeDir, ".transitive", "config.json")
	}
	return filepath.Join(os.TempDir(), "transitive", "config.json")
}

// LoadFromFile reads ~/.transitive/config.json and applies values to the config.
// Missing file is not an error — only non-zero values from the file override defaults.
func (c *Config) LoadFromFile() error {
	path := ConfigPath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config: %w", err)
	}

	var file Config
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	if file.Port != 0 {
		c.Port = file.Port
	}
	if file.RelayURL != "" {
		c.RelayURL = file.RelayURL
	}
	if file.NoRelay {
		c.NoRelay = true
	}
	if file.HookPort != 0 {
		c.HookPort = file.HookPort
	}
	if file.ClaudePath != "" {
		c.ClaudePath = file.ClaudePath
	}
	if file.OpenCodePath != "" {
		c.OpenCodePath = file.OpenCodePath
	}
	if file.ProjectDir != "" {
		c.ProjectDir = file.ProjectDir
	}
	if file.EnableClaude {
		c.EnableClaude = true
	}

	log.Printf("loaded config from %s", path)
	return nil
}
