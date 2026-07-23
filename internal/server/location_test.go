package server

import (
	"net/http"
	"testing"
)

func nop() http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
}

func TestLocationMatch(t *testing.T) {
	mk := func(mod, pat string) *route {
		return &route{pattern: pat, modifier: mod, handler: nop()}
	}
	exact := mk("=", "/exact")
	static := mk("^~", "/static/")
	php := mk("~", `\.php$`)
	img := mk("~*", `\.(jpg|png)$`)
	docs := mk("", "/docs/")
	root := mk("", "/")

	set, err := newLocationSet([]*route{exact, static, php, img, docs, root})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want *route
		desc string
	}{
		{"/exact", exact, "exact match wins"},
		{"/exact/sub", root, "exact does not prefix-match"},
		{"/static/app.php", static, "^~ prefix short-circuits regex"},
		{"/app.php", php, "case-sensitive regex"},
		{"/app.PHP", root, "~ is case-sensitive"},
		{"/pics/A.PNG", img, "~* is case-insensitive"},
		{"/docs/guide", docs, "longest plain prefix"},
		{"/docs/a.php", php, "regex beats plain prefix"},
		{"/other", root, "fallback to /"},
	}
	for _, c := range cases {
		if got := set.match(c.path); got != c.want {
			t.Errorf("%s: match(%q) = %+v, want %+v", c.desc, c.path, got, c.want)
		}
	}
}

func TestLocationLongestPrefixWins(t *testing.T) {
	short := &route{pattern: "/a/", handler: nop()}
	long := &route{pattern: "/a/b/", handler: nop()}
	set, err := newLocationSet([]*route{short, long})
	if err != nil {
		t.Fatal(err)
	}
	if set.match("/a/b/c") != long {
		t.Error("longest prefix should win")
	}
	if set.match("/a/x") != short {
		t.Error("short prefix should catch the rest")
	}
	if set.match("/z") != nil {
		t.Error("no match expected")
	}
}

func TestVHostMatch(t *testing.T) {
	mkv := func(names ...string) *vhost {
		ms, err := buildNameMatchers(names)
		if err != nil {
			t.Fatal(err)
		}
		return &vhost{names: ms}
	}
	exact := mkv("example.com")
	wildStart := mkv("*.example.com")
	wildEnd := mkv("www.*")
	re := mkv(`~^api\d+\.`)
	def := mkv()

	g := &hostGroup{vhosts: []*vhost{exact, wildStart, wildEnd, re}, def: def}

	cases := []struct {
		host string
		want *vhost
	}{
		{"example.com", exact},
		{"EXAMPLE.COM:8080", exact},
		{"foo.example.com", wildStart},
		{"www.other.org", wildEnd},
		{"api7.somewhere.io", re},
		{"unknown.net", def},
	}
	for _, c := range cases {
		if got := g.match(c.host); got != c.want {
			t.Errorf("match(%q) wrong vhost", c.host)
		}
	}
}
