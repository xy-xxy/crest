package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("http:\n  servers: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFindConfigOrder pins the probe order a double-clicked exe relies on:
// YAML before .conf, working directory before the executable's directory.
func TestFindConfigOrder(t *testing.T) {
	t.Run("prefers yml over conf", func(t *testing.T) {
		d := t.TempDir()
		want := write(t, d, "crest.yml")
		write(t, d, "crest.conf")
		got, err := findConfig([]string{d})
		if err != nil || got != want {
			t.Errorf("findConfig = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("accepts yaml spelling", func(t *testing.T) {
		d := t.TempDir()
		want := write(t, d, "crest.yaml")
		got, err := findConfig([]string{d})
		if err != nil || got != want {
			t.Errorf("findConfig = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("falls back to conf", func(t *testing.T) {
		d := t.TempDir()
		want := write(t, d, "crest.conf")
		got, err := findConfig([]string{d})
		if err != nil || got != want {
			t.Errorf("findConfig = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("earlier directory wins", func(t *testing.T) {
		first, second := t.TempDir(), t.TempDir()
		want := write(t, first, "crest.conf") // even a .conf here beats...
		write(t, second, "crest.yml")         // ...a .yml one directory later
		got, err := findConfig([]string{first, second})
		if err != nil || got != want {
			t.Errorf("findConfig = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("second directory used when first is empty", func(t *testing.T) {
		first, second := t.TempDir(), t.TempDir()
		want := write(t, second, "crest.yml")
		got, err := findConfig([]string{first, second})
		if err != nil || got != want {
			t.Errorf("findConfig = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("a directory named like a config is skipped", func(t *testing.T) {
		d := t.TempDir()
		if err := os.Mkdir(filepath.Join(d, "crest.yml"), 0755); err != nil {
			t.Fatal(err)
		}
		want := write(t, d, "crest.conf")
		got, err := findConfig([]string{d})
		if err != nil || got != want {
			t.Errorf("findConfig = %q, %v; want %q", got, err, want)
		}
	})
}

// TestFindConfigNotFound checks the message lists every path tried — the only
// feedback someone gets when a double-clicked exe cannot start.
func TestFindConfigNotFound(t *testing.T) {
	d := t.TempDir()
	_, err := findConfig([]string{d})
	if err == nil {
		t.Fatal("expected an error when no config exists")
	}
	for _, name := range configNames {
		if !strings.Contains(err.Error(), filepath.Join(d, name)) {
			t.Errorf("error does not mention %q: %v", name, err)
		}
	}
	if !strings.Contains(err.Error(), "-c") {
		t.Errorf("error should point at -c: %v", err)
	}
}

// TestResolveConfigPathExplicit makes sure -c still wins outright and is never
// reported as auto-detected.
func TestResolveConfigPathExplicit(t *testing.T) {
	got, auto, err := resolveConfigPath("/some/where/custom.yml")
	if err != nil || got != "/some/where/custom.yml" || auto {
		t.Errorf("resolveConfigPath = %q, auto=%v, %v", got, auto, err)
	}
}
