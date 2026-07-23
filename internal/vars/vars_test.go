package vars

import (
	"net/http/httptest"
	"testing"
)

func TestValue(t *testing.T) {
	r := httptest.NewRequest("GET", "http://Example.com:8080/path/to?x=1&y=2", nil)
	r.Header.Set("Referer", "http://from.example")
	r.RemoteAddr = "10.1.2.3:5555"

	cases := map[string]string{
		"uri":            "/path/to",
		"request_uri":    "/path/to?x=1&y=2",
		"args":           "x=1&y=2",
		"is_args":        "?",
		"host":           "example.com",
		"http_host":      "Example.com:8080",
		"remote_addr":    "10.1.2.3",
		"request_method": "GET",
		"scheme":         "http",
		"http_referer":   "http://from.example",
		"arg_y":          "2",
		"unknown_var":    "",
	}
	for name, want := range cases {
		if got := Value(r, name); got != want {
			t.Errorf("$%s = %q, want %q", name, got, want)
		}
	}
}

func TestTemplate(t *testing.T) {
	r := httptest.NewRequest("GET", "http://h.test/a?q=1", nil)
	cases := []struct {
		tmpl string
		want string
	}{
		{"plain text", "plain text"},
		{"$uri", "/a"},
		{"https://$host$request_uri", "https://h.test/a?q=1"},
		{"${uri}.html", "/a.html"},
		{"cost: $100", "cost: $100"}, // $1 has no capture context here → empty... see below
		{"end$", "end$"},
	}
	for _, c := range cases {
		got := Parse(c.tmpl).Expand(Request(r))
		if c.tmpl == "cost: $100" {
			// $100 parses as var "100" which resolves empty via request resolver
			if got != "cost: " {
				t.Errorf("Expand(%q) = %q, want %q", c.tmpl, got, "cost: ")
			}
			continue
		}
		if got != c.want {
			t.Errorf("Expand(%q) = %q, want %q", c.tmpl, got, c.want)
		}
	}
}
