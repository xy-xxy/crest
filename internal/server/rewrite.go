package server

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"gonx/internal/config"
	"gonx/internal/vars"
)

type rewriteRule struct {
	re   *regexp.Regexp
	repl *vars.Template
	flag string
}

func compileRewrites(rs []config.Rewrite) ([]*rewriteRule, error) {
	var out []*rewriteRule
	for _, r := range rs {
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return nil, fmt.Errorf("rewrite (line %d): invalid regex %q: %v", r.Line, r.Regex, err)
		}
		out = append(out, &rewriteRule{re: re, repl: vars.Parse(r.Replacement), flag: r.Flag})
	}
	return out, nil
}

type rwOutcome int

const (
	rwNone    rwOutcome = iota
	rwChanged           // URI rewritten: re-run location matching
	rwBreak             // stop rewriting, serve in the current location
	rwDone              // response already written (redirect)
)

// applyRewrites runs rules against r.URL.Path in order, mutating the request
// URI in place. Replacement supports $1-$9 captures plus request variables.
func applyRewrites(rules []*rewriteRule, w http.ResponseWriter, r *http.Request) rwOutcome {
	changed := false
	for _, rule := range rules {
		m := rule.re.FindStringSubmatch(r.URL.Path)
		if m == nil {
			continue
		}
		raw := rule.repl.Expand(func(name string) string {
			if len(name) == 1 && name[0] >= '1' && name[0] <= '9' {
				if i := int(name[0] - '0'); i < len(m) {
					return m[i]
				}
				return ""
			}
			return vars.Value(r, name)
		})
		external := strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
		// nginx appends the previous request arguments to the replacement;
		// a trailing "?" suppresses that. This applies to an absolute
		// replacement (an external redirect) just as it does to a local one.
		dropArgs := strings.HasSuffix(raw, "?")
		trimmed := strings.TrimSuffix(raw, "?")
		path, args := trimmed, ""
		if i := strings.IndexByte(trimmed, '?'); i >= 0 {
			path, args = trimmed[:i], trimmed[i+1:]
			if !dropArgs && args != "" && r.URL.RawQuery != "" {
				args += "&" + r.URL.RawQuery
			}
		} else if !dropArgs {
			args = r.URL.RawQuery
		}
		target := path
		if args != "" {
			target += "?" + args
		}
		if !external {
			r.URL.Path, r.URL.RawQuery = path, args
			changed = true
		}
		if external || rule.flag == "redirect" || rule.flag == "permanent" {
			code := http.StatusFound
			if rule.flag == "permanent" {
				code = http.StatusMovedPermanently
			}
			http.Redirect(w, r, target, code)
			return rwDone
		}
		switch rule.flag {
		case "last":
			return rwChanged
		case "break":
			return rwBreak
		}
	}
	if changed {
		return rwChanged
	}
	return rwNone
}
