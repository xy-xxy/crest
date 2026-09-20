package server

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"crest/internal/handler"
	"crest/internal/vars"
)

type route struct {
	pattern  string
	modifier string // "", "=", "~", "~*", "^~"
	noRegex  bool   // ^~: skip regex phase when this prefix wins
	re       *regexp.Regexp
	handler  http.Handler
	rewrites []*rewriteRule
	tryFiles []*vars.Template
	static   *handler.Static // file mapping for try_files; nil for proxy/return
	isProxy  bool
}

// resolveTryFiles picks the first existing candidate. The last argument is an
// internal redirect (redo=true), a "=code" response, or a @named location
// (named set), per nginx semantics.
func (rt *route) resolveTryFiles(r *http.Request) (uri string, code int, redo bool, named string) {
	last := len(rt.tryFiles) - 1
	for i, tmpl := range rt.tryFiles {
		val := tmpl.Expand(vars.Request(r))
		if i == last {
			switch {
			case strings.HasPrefix(val, "@"):
				return "", 0, false, val
			case strings.HasPrefix(val, "="):
				if c, err := strconv.Atoi(val[1:]); err == nil {
					return "", c, false, ""
				}
			}
			return val, 0, true, ""
		}
		fi, err := os.Stat(rt.static.FilePath(val))
		if err != nil {
			continue
		}
		if strings.HasSuffix(val, "/") == fi.IsDir() {
			return val, 0, false, ""
		}
	}
	return "", http.StatusNotFound, false, ""
}

type locationSet struct {
	exact    map[string]*route
	prefixes []*route // sorted longest-first
	regexes  []*route // config order
	named    map[string]*route
}

func newLocationSet(routes []*route) (*locationSet, error) {
	s := &locationSet{exact: map[string]*route{}}
	for _, r := range routes {
		switch r.modifier {
		case "=":
			if _, dup := s.exact[r.pattern]; dup {
				return nil, fmt.Errorf("duplicate location = %q", r.pattern)
			}
			s.exact[r.pattern] = r
		case "~", "~*":
			pat := r.pattern
			if r.modifier == "~*" {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("location %q: invalid regex: %v", r.pattern, err)
			}
			r.re = re
			s.regexes = append(s.regexes, r)
		default:
			r.noRegex = r.modifier == "^~"
			s.prefixes = append(s.prefixes, r)
		}
	}
	sort.SliceStable(s.prefixes, func(i, j int) bool {
		return len(s.prefixes[i].pattern) > len(s.prefixes[j].pattern)
	})
	return s, nil
}

// match implements nginx location selection: exact first, then the longest
// matching prefix (which short-circuits regexes if declared ^~), then
// regexes in config order, falling back to the best prefix.
func (s *locationSet) match(path string) *route {
	if r, ok := s.exact[path]; ok {
		return r
	}
	var bestPrefix *route
	for _, r := range s.prefixes {
		if strings.HasPrefix(path, r.pattern) {
			bestPrefix = r
			break
		}
	}
	if bestPrefix != nil && bestPrefix.noRegex {
		return bestPrefix
	}
	for _, r := range s.regexes {
		if r.re.MatchString(path) {
			return r
		}
	}
	return bestPrefix
}
