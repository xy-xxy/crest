package handler

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"crest/internal/logging"
)

// Static serves files with nginx root/alias semantics:
// root maps the full URI under Root; alias replaces the matched location
// prefix with Alias.
type Static struct {
	Root       string
	Alias      string
	Prefix     string // the location pattern this handler is mounted at
	Index      []string
	Autoindex  bool
	GzipStatic bool // serve a precompressed <file>.gz when the client accepts gzip
	ErrLog     *logging.ErrorLog
}

func (s *Static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "405 Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	fsPath := s.FilePath(r.URL.Path)
	if fsPath == "" {
		// No root/alias in scope — the shape of a proxy-only server whose
		// unmatched requests fall through here. nginx would look under its
		// built-in default root and 404; answering 500 turned every stray
		// /favicon.ico into a server error. Warn (filtered out at the
		// default error level) and 404.
		if s.Root == "" && s.Alias == "" {
			s.ErrLog.Logf(logging.LevelWarn, `no "root" configured for request %q`, r.URL.Path)
		} else {
			s.ErrLog.Logf(logging.LevelWarn, "request %q maps outside the configured root, denied", r.URL.Path)
		}
		http.NotFound(w, r)
		return
	}

	fi, err := os.Stat(fsPath)
	if err != nil {
		s.fail(w, r, err, fsPath)
		return
	}
	if fi.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		for _, idx := range s.Index {
			cand := filepath.Join(fsPath, idx)
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				s.serveFile(w, r, cand, st)
				return
			}
		}
		if s.Autoindex {
			s.serveListing(w, r, fsPath)
			return
		}
		// autoindex is off, matching nginx's default
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	s.serveFile(w, r, fsPath, fi)
}

// serveListing renders an nginx-style directory index.
func (s *Static) serveListing(w http.ResponseWriter, r *http.Request, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.fail(w, r, err, dir)
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})
	title := html.EscapeString(r.URL.Path)
	var b strings.Builder
	fmt.Fprintf(&b, "<html>\n<head><title>Index of %s</title></head>\n<body>\n<h1>Index of %s</h1><hr><pre>", title, title)
	b.WriteString("<a href=\"../\">../</a>\n")
	for _, e := range entries {
		name := e.Name()
		href := (&url.URL{Path: name}).EscapedPath()
		disp := html.EscapeString(name)
		if e.IsDir() {
			href += "/"
			disp += "/"
		}
		info, ierr := e.Info()
		size, mtime := "-", ""
		if ierr == nil {
			mtime = info.ModTime().Format("02-Jan-2006 15:04")
			if !e.IsDir() {
				size = strconv.FormatInt(info.Size(), 10)
			}
		}
		pad := 50 - len(disp)
		if pad < 1 {
			pad = 1
		}
		fmt.Fprintf(&b, "<a href=%q>%s</a>%s%s %19s\n", href, disp, strings.Repeat(" ", pad), mtime, size)
	}
	b.WriteString("</pre><hr></body>\n</html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, b.String())
}

// FilePath maps a URI to a filesystem path using root/alias semantics.
// A backslash counts as a URI separator before cleaning: path.Clean leaves
// "..\\" alone (it is one opaque element to it), but Windows filepath.Join
// then honours it as a parent hop and the request escapes the configured
// root. Browsers already normalise "\\" to "/" per the WHATWG URL spec, so
// only a hand-crafted "%5C" request is affected. withinBase re-checks the
// mapped path as a second line of defence.
// Returns "" when neither root nor alias is configured, and when the mapped
// path would land outside the base directory.
func (s *Static) FilePath(uriPath string) string {
	upath := path.Clean("/" + strings.ReplaceAll(uriPath, `\`, "/"))
	base, rest := s.Root, upath
	if s.Alias != "" {
		base = s.Alias
		rest = strings.TrimPrefix(upath, strings.TrimSuffix(s.Prefix, "/"))
	}
	if base == "" {
		return ""
	}
	full := filepath.Join(base, filepath.FromSlash(rest))
	if !withinBase(base, full) {
		return ""
	}
	return full
}

// withinBase reports whether p is base itself or sits inside it.
func withinBase(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func (s *Static) serveFile(w http.ResponseWriter, r *http.Request, fsPath string, fi os.FileInfo) {
	// gzip_static: prefer <file>.gz if it exists and the client accepts gzip
	if s.GzipStatic && r.Method != http.MethodHead &&
		strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		if gz, err := os.Open(fsPath + ".gz"); err == nil {
			defer gz.Close()
			if gzi, err := gz.Stat(); err == nil && !gzi.IsDir() {
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Add("Vary", "Accept-Encoding")
				// name the served content by the original path so ServeContent
				// picks the real Content-Type (not application/gzip)
				http.ServeContent(w, r, fi.Name(), fi.ModTime(), gz)
				return
			}
		}
	}
	f, err := os.Open(fsPath)
	if err != nil {
		s.fail(w, r, err, fsPath)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

func (s *Static) fail(w http.ResponseWriter, r *http.Request, err error, fsPath string) {
	switch {
	case os.IsNotExist(err):
		s.ErrLog.Logf(logging.LevelInfo, "open %q failed (404): %v", fsPath, err)
		http.NotFound(w, r)
	case os.IsPermission(err):
		s.ErrLog.Logf(logging.LevelWarn, "open %q failed (403): %v", fsPath, err)
		http.Error(w, "403 Forbidden", http.StatusForbidden)
	default:
		s.ErrLog.Logf(logging.LevelError, "open %q failed (500): %v", fsPath, err)
		http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
	}
}
