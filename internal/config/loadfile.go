package config

import (
	"fmt"
	"os"
	"strings"
)

// LoadFile reads and parses a configuration file, choosing the format by
// extension (.yml/.yaml → YAML) and falling back to content sniffing so a
// YAML document also works under a .conf name; everything else uses the
// nginx-compatible syntax.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(path)
	isYAML := strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml") || looksLikeYAML(data)
	if isYAML {
		cfg, err := LoadYAML(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return cfg, nil
	}
	ds, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg, err := Load(ds)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// looksLikeYAML reports whether config data is gonx's YAML format rather than
// nginx syntax, by testing whether the first meaningful line is a top-level
// YAML key. nginx blocks open with "http {" (no colon) and directives end in
// ";", so the two never collide.
func looksLikeYAML(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return strings.HasPrefix(t, "http:") ||
			strings.HasPrefix(t, "stream:") ||
			strings.HasPrefix(t, "error_log:")
	}
	return false
}
