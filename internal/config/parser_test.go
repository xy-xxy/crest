package config

import (
	"strings"
	"testing"
)

func TestParseBasic(t *testing.T) {
	src := `
# comment
worker_processes 1;

http {
    server {
        listen 8080;
        server_name example.com *.example.org;
        location / {
            root /var/www;
        }
    }
}
`
	ds, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 {
		t.Fatalf("want 2 top-level directives, got %d", len(ds))
	}
	if ds[0].Name != "worker_processes" || len(ds[0].Args) != 1 || ds[0].Args[0] != "1" {
		t.Errorf("bad directive: %+v", ds[0])
	}
	if ds[0].Block != nil {
		t.Error("worker_processes should not have a block")
	}
	httpD := ds[1]
	if httpD.Name != "http" || httpD.Block == nil {
		t.Fatalf("bad http block: %+v", httpD)
	}
	srv := httpD.Block[0]
	if srv.Name != "server" || len(srv.Block) != 3 {
		t.Fatalf("bad server block: %+v", srv)
	}
	if srv.Block[1].Name != "server_name" || len(srv.Block[1].Args) != 2 {
		t.Errorf("bad server_name: %+v", srv.Block[1])
	}
	loc := srv.Block[2]
	if loc.Name != "location" || loc.Args[0] != "/" || loc.Block[0].Name != "root" {
		t.Errorf("bad location: %+v", loc)
	}
}

func TestParseQuotes(t *testing.T) {
	src := `log_format main '$remote_addr "$request"' " and \"more\"";`
	ds, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds[0].Args) != 3 {
		t.Fatalf("want 3 args, got %v", ds[0].Args)
	}
	if ds[0].Args[1] != `$remote_addr "$request"` {
		t.Errorf("single-quoted arg wrong: %q", ds[0].Args[1])
	}
	if ds[0].Args[2] != ` and "more"` {
		t.Errorf("escaped double-quoted arg wrong: %q", ds[0].Args[2])
	}
}

func TestParseSemicolonInWord(t *testing.T) {
	ds, err := Parse(`a b;c d;`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[0].Args[0] != "b" || ds[1].Name != "c" {
		t.Errorf("semicolon should split words: %+v", ds)
	}
}

func TestParseEmptyBlock(t *testing.T) {
	ds, err := Parse(`events {}`)
	if err != nil {
		t.Fatal(err)
	}
	if ds[0].Block == nil || len(ds[0].Block) != 0 {
		t.Errorf("empty block should be non-nil empty: %+v", ds[0])
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{`a b`, "unexpected end of file"},
		{`http {`, `expecting "}"`},
		{`}`, `unexpected "}"`},
		{`;`, `unexpected ";"`},
		{`a "unclosed`, "unterminated"},
		{`http { a }`, `unexpected "}"`},
	}
	for _, c := range cases {
		_, err := Parse(c.src)
		if err == nil {
			t.Errorf("%q: want error containing %q, got nil", c.src, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: want error containing %q, got %q", c.src, c.want, err)
		}
	}
}
