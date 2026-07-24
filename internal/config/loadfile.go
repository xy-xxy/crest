package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadFile reads and parses a configuration file, choosing the format by
// extension (.yml/.yaml → YAML) and falling back to content sniffing so a
// YAML document also works under a .conf name; everything else uses the
// nginx-compatible syntax. Relative paths inside the config (root, alias,
// certificates, htpasswd, logs, cache dirs) resolve against the config
// file's directory, so behavior does not depend on the working directory.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg *Config
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml") || looksLikeYAML(data) {
		if cfg, err = LoadYAML(data); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else {
		ds, err := Parse(string(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if cfg, err = Load(ds); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	base := filepath.Dir(path)
	if abs, err := filepath.Abs(path); err == nil {
		base = filepath.Dir(abs)
	}
	cfg.resolvePaths(base)
	return cfg, nil
}

// pathIsSpecial reports pseudo-paths that must not be joined to a base dir.
func pathIsSpecial(p string) bool {
	switch p {
	case "", "off", "-", "stdout", "stderr", "/dev/stdout", "/dev/stderr":
		return true
	}
	return false
}

// resolvePaths anchors relative filesystem paths to the config directory and
// warns about targets that do not exist, so "-t" surfaces broken roots
// before the first 404 does.
func (c *Config) resolvePaths(base string) {
	abs := func(p string) string {
		if pathIsSpecial(p) || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	warnMissing := func(kind, p string) {
		if pathIsSpecial(p) {
			return
		}
		if _, err := os.Stat(p); err != nil {
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s %q does not exist", kind, p))
		}
	}
	h := c.HTTP
	if h == nil {
		return
	}
	h.Root = abs(h.Root)
	warnMissing("root", h.Root)
	h.AccessLog = abs(h.AccessLog)
	h.ErrorLog = abs(h.ErrorLog)
	for k, v := range h.CachePaths {
		h.CachePaths[k] = abs(v) // created on demand, no warning
	}
	if h.Auth != nil {
		h.Auth.UserFile = abs(h.Auth.UserFile)
		warnMissing("auth_basic_user_file", h.Auth.UserFile)
	}
	for _, s := range h.Servers {
		s.Root = abs(s.Root)
		warnMissing("root", s.Root)
		s.AccessLog = abs(s.AccessLog)
		s.SSLCert = abs(s.SSLCert)
		warnMissing("ssl_certificate", s.SSLCert)
		s.SSLKey = abs(s.SSLKey)
		warnMissing("ssl_certificate_key", s.SSLKey)
		if s.Auth != nil {
			s.Auth.UserFile = abs(s.Auth.UserFile)
			warnMissing("auth_basic_user_file", s.Auth.UserFile)
		}
		for _, l := range s.Locations {
			l.Root = abs(l.Root)
			warnMissing("root", l.Root)
			l.Alias = abs(l.Alias)
			warnMissing("alias", l.Alias)
			if l.Auth != nil {
				l.Auth.UserFile = abs(l.Auth.UserFile)
				warnMissing("auth_basic_user_file", l.Auth.UserFile)
			}
		}
	}
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
