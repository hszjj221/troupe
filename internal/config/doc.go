// Package config loads optional YAML configuration for troupe. Config contains
// default_agent, roles (role name to agent name), and agents (binary overrides).
// Lookup order is --config, TROUPE_CONFIG, ./troupe.yaml, then config.yaml under
// TROUPE_HOME or .troupe. Missing explicit files are errors; missing defaults
// yield an empty config. Agent selection is --agent, role binding, default_agent.
package config
