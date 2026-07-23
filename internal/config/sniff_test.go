package config

import "testing"

func TestLooksLikeYAML(t *testing.T) {
	cases := []struct {
		name string
		src  string
		yaml bool
	}{
		{"yaml http", "# comment\n\nhttp:\n  servers: []\n", true},
		{"yaml stream", "stream:\n  servers: []\n", true},
		{"yaml error_log first", "error_log: {path: x}\nhttp:\n", true},
		{"nginx block", "worker_processes 1;\nhttp {\n}\n", false},
		{"nginx http block", "http {\n  server {}\n}\n", false},
		{"nginx error_log directive", "error_log /var/log/e.log;\nhttp {}\n", false},
		{"empty", "\n\n# only comments\n", false},
	}
	for _, c := range cases {
		if got := looksLikeYAML([]byte(c.src)); got != c.yaml {
			t.Errorf("%s: looksLikeYAML = %v, want %v", c.name, got, c.yaml)
		}
	}
}
