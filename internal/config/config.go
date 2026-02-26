package config

import "fmt"

type Config struct {
	Port       int
	RelayURL   string
	NoRelay    bool
	AgentID    string
	Secret     string
	HookPort   int
	ClaudePath string
}

func (c *Config) HookAddr() string {
	return fmt.Sprintf("localhost:%d", c.HookPort)
}

func (c *Config) ListenAddr() string {
	return fmt.Sprintf(":%d", c.Port)
}

func DefaultConfig() *Config {
	return &Config{
		Port:       7865,
		RelayURL:   "wss://claudette-relay.transitive.dev",
		HookPort:   7865,
		ClaudePath: "claude",
	}
}
