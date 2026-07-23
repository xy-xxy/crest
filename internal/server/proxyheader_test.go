package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestProxySetHeaderAndCookiePath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "sid=abc; Path=/prod-api/; HttpOnly")
		fmt.Fprintf(w, "host=%s custom=%s", r.Host, r.Header.Get("X-Custom"))
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        location /prod-api/ {
            proxy_set_header Host $host;
            proxy_set_header X-Custom "from-gonx";
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_pass http://%s/;
            proxy_cookie_path /prod-api/ /;
        }
    }
}
`, bu.Host)

	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/prod-api/login", nil)
	req.Host = "myapp.example"
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)

	want := "host=myapp.example custom=from-gonx"
	if got := string(body[:n]); got != want {
		t.Errorf("proxy_set_header: got %q, want %q", got, want)
	}
	if got := resp.Header.Get("Set-Cookie"); got != "sid=abc; Path=/; HttpOnly" {
		t.Errorf("proxy_cookie_path: got %q", got)
	}
}
