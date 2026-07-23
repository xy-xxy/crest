package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelNotice
	LevelWarn
	LevelError
	LevelCrit
)

var levelNames = map[Level]string{
	LevelDebug:  "debug",
	LevelInfo:   "info",
	LevelNotice: "notice",
	LevelWarn:   "warn",
	LevelError:  "error",
	LevelCrit:   "crit",
}

func ParseLevel(s string) (Level, bool) {
	for lv, name := range levelNames {
		if name == s {
			return lv, true
		}
	}
	return 0, false
}

type ErrorLog struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	path   string
	min    Level
}

// OpenError creates an error log. Path "" or "stderr" writes to stderr.
func OpenError(path string, min Level) (*ErrorLog, error) {
	switch path {
	case "", "stderr", "/dev/stderr":
		return &ErrorLog{w: os.Stderr, min: min}, nil
	case "-", "stdout", "/dev/stdout":
		return &ErrorLog{w: os.Stdout, min: min}, nil
	}
	f, err := openLogFile(path)
	if err != nil {
		return nil, err
	}
	return &ErrorLog{w: f, closer: f, path: path, min: min}, nil
}

// OpenErrorWriter creates an error log over an arbitrary writer (e.g. a tee of
// stderr and a console file). Reopen is a no-op for such logs.
func OpenErrorWriter(w io.Writer, min Level) *ErrorLog {
	return &ErrorLog{w: w, min: min}
}

// Reopen reopens the backing file for log rotation. No-op for std streams.
func (l *ErrorLog) Reopen() error {
	if l == nil || l.path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closer != nil {
		l.closer.Close()
	}
	f, err := openLogFile(l.path)
	if err != nil {
		return err
	}
	l.w = f
	l.closer = f
	return nil
}

func (l *ErrorLog) Logf(lv Level, format string, args ...any) {
	if l == nil || lv < l.min {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "%s [%s] %s\n",
		time.Now().Format("2006/01/02 15:04:05"), levelNames[lv], fmt.Sprintf(format, args...))
}

func (l *ErrorLog) Close() {
	if l == nil || l.closer == nil {
		return
	}
	l.closer.Close()
}

func openLogFile(path string) (*os.File, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
}
