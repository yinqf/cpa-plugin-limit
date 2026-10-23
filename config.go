package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxLimit = 10000

const (
	displayFingerprint = "fingerprint"
	displayMaskedEmail = "masked_email"
	displayEmail       = "email"
	routingCapacity    = "capacity-aware"
	routingGuard       = "guard-only"
)

type limits struct {
	RequestsPerMinute int `yaml:"requests_per_minute" json:"requests_per_minute"`
	MaxConcurrency    int `yaml:"max_concurrency" json:"max_concurrency"`
}

type limitOverride struct {
	RequestsPerMinute *int `yaml:"requests_per_minute" json:"requests_per_minute,omitempty"`
	MaxConcurrency    *int `yaml:"max_concurrency" json:"max_concurrency,omitempty"`
}

type config struct {
	limits             `yaml:",inline"`
	AuthIndexes        []string                 `yaml:"auth_indexes" json:"auth_indexes"`
	AccountGroups      map[string]string        `yaml:"account_groups" json:"account_groups"`
	AccountLimits      map[string]limitOverride `yaml:"account_limits" json:"account_limits"`
	AccountDisplay     string                   `yaml:"account_display" json:"account_display"`
	RoutingMode        string                   `yaml:"routing_mode" json:"routing_mode"`
	SessionAffinityTTL int                      `yaml:"session_affinity_ttl_seconds" json:"session_affinity_ttl_seconds"`
}

type registerRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

func defaultConfig() config {
	return config{limits: limits{RequestsPerMinute: 5, MaxConcurrency: 1}, AccountDisplay: displayEmail, RoutingMode: routingCapacity, SessionAffinityTTL: 3600}
}

func parseConfig(raw []byte) (config, error) {
	var req registerRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return config{}, fmt.Errorf("decode registration: %w", err)
	}
	if req.SchemaVersion < 2 {
		return config{}, fmt.Errorf("host RPC schema version 2 or newer is required for termination and lifecycle completion")
	}
	cfg := defaultConfig()
	var document yaml.Node
	if err := yaml.Unmarshal(req.ConfigYAML, &document); err != nil {
		return config{}, fmt.Errorf("decode plugin configuration: %w", err)
	}
	if len(document.Content) > 0 {
		root := document.Content[0]
		if err := validateIntegerFields(root); err != nil {
			return config{}, err
		}
		if err := root.Decode(&cfg); err != nil {
			return config{}, fmt.Errorf("decode plugin configuration: %w", err)
		}
	}
	if err := cfg.limits.validate(); err != nil {
		return config{}, err
	}
	cfg.AccountDisplay = strings.TrimSpace(cfg.AccountDisplay)
	if cfg.AccountDisplay == "" {
		cfg.AccountDisplay = displayEmail
	}
	switch cfg.AccountDisplay {
	case displayFingerprint, displayMaskedEmail, displayEmail:
	default:
		return config{}, fmt.Errorf("account_display must be fingerprint, masked_email, or email")
	}
	cfg.RoutingMode = strings.TrimSpace(cfg.RoutingMode)
	if cfg.RoutingMode == "" {
		cfg.RoutingMode = routingCapacity
	}
	if cfg.RoutingMode != routingCapacity && cfg.RoutingMode != routingGuard {
		return config{}, fmt.Errorf("routing_mode must be capacity-aware or guard-only")
	}
	if cfg.SessionAffinityTTL < 0 || cfg.SessionAffinityTTL > 86400 {
		return config{}, fmt.Errorf("session_affinity_ttl_seconds must be between 0 and 86400; zero disables affinity")
	}
	seen := make(map[string]bool)
	for i, value := range cfg.AuthIndexes {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return config{}, fmt.Errorf("auth_indexes must contain unique non-empty credential indexes or IDs")
		}
		seen[value] = true
		cfg.AuthIndexes[i] = value
	}
	for credential, group := range cfg.AccountGroups {
		if credential == "" || strings.TrimSpace(credential) != credential || group == "" || strings.TrimSpace(group) != group {
			return config{}, fmt.Errorf("account_groups keys and values must be non-empty without surrounding whitespace")
		}
	}
	for account, override := range cfg.AccountLimits {
		if account == "" || strings.TrimSpace(account) != account {
			return config{}, fmt.Errorf("account_limits keys must be non-empty without surrounding whitespace")
		}
		if err := override.apply(cfg.limits).validate(); err != nil {
			return config{}, fmt.Errorf("account_limits[%q]: %w", account, err)
		}
	}
	return cfg, nil
}

// yaml.v3 otherwise silently truncates fractional numbers decoded into ints.
func validateIntegerFields(node *yaml.Node) error {
	var fields map[string]any
	if err := node.Decode(&fields); err != nil {
		return fmt.Errorf("decode plugin configuration: %w", err)
	}
	return validateLimitValues(fields)
}

func validateLimitValues(fields map[string]any) error {
	for _, key := range []string{"requests_per_minute", "max_concurrency", "session_affinity_ttl_seconds"} {
		switch fields[key].(type) {
		case nil, int, int64, uint64:
		default:
			return fmt.Errorf("%s must be an integer", key)
		}
	}
	if overrides, ok := fields["account_limits"].(map[string]any); ok {
		for _, value := range overrides {
			if values, ok := value.(map[string]any); ok {
				if err := validateLimitValues(values); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (l limits) validate() error {
	if l.RequestsPerMinute < 0 || l.RequestsPerMinute > maxLimit || l.MaxConcurrency < 0 || l.MaxConcurrency > maxLimit {
		return fmt.Errorf("requests_per_minute and max_concurrency must be integers between 0 and %d; zero disables that limit", maxLimit)
	}
	return nil
}

func (o limitOverride) apply(base limits) limits {
	if o.RequestsPerMinute != nil {
		base.RequestsPerMinute = *o.RequestsPerMinute
	}
	if o.MaxConcurrency != nil {
		base.MaxConcurrency = *o.MaxConcurrency
	}
	return base
}

func (c config) accountLimits(name string) limits {
	return c.AccountLimits[name].apply(c.limits)
}

func (c config) includes(index, id string) bool {
	if len(c.AuthIndexes) == 0 {
		return true
	}
	for _, selected := range c.AuthIndexes {
		if selected == index || selected == id {
			return true
		}
	}
	return false
}
