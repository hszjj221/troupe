package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config binds task roles to agents and optionally overrides their binaries.
type Config struct {
	DefaultAgent string                 `yaml:"default_agent"`
	Roles        map[string]string      `yaml:"roles"`
	Agents       map[string]AgentConfig `yaml:"agents"`
}

// AgentConfig overrides the executable for a built-in agent.
type AgentConfig struct {
	Binary string `yaml:"binary"`
}

// ResolvePath selects one file, without merging configurations. flagPath and
// envPath are explicit; otherwise troupe.yaml in cwd precedes home/config.yaml
// (home defaults to .troupe). Load reports errors for unreadable candidates.
func ResolvePath(flagPath, envPath, home string) (path string, explicit bool) {
	if flagPath != "" {
		return flagPath, true
	}
	if envPath != "" {
		return envPath, true
	}
	if _, err := os.Stat("troupe.yaml"); !errors.Is(err, os.ErrNotExist) {
		return "troupe.yaml", false
	}
	if home == "" {
		home = ".troupe"
	}
	return filepath.Join(home, "config.yaml"), false
}

// Load reads YAML, ignoring unknown keys. Only a missing optional file is
// treated as empty; malformed YAML and all other read errors are returned.
func Load(path string, explicit bool) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	return cfg, nil
}

// ResolveAgent applies CLI override, role binding, then default_agent. The
// caller validates the selected name against its supported agents.
func ResolveAgent(cfg Config, agentName, role string) (string, error) {
	if agentName != "" {
		return agentName, nil
	}
	if bound, ok := cfg.Roles[role]; role != "" && ok {
		return bound, nil
	}
	if cfg.DefaultAgent != "" {
		return cfg.DefaultAgent, nil
	}
	return "", errors.New("need --agent or config with a role binding or default_agent")
}
