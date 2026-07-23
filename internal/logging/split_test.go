package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSplitAccessLog(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, "success.log")
	fp := filepath.Join(dir, "fail.log")
	s, err := OpenSplit(sp, fp, "$request $status")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.Log(AccessRecord{Request: "GET /a", Status: 200, Time: now}) // success
	s.Log(AccessRecord{Request: "GET /b", Status: 302, Time: now}) // success (<400)
	s.Log(AccessRecord{Request: "GET /c", Status: 404, Time: now}) // fail
	s.Log(AccessRecord{Request: "GET /d", Status: 503, Time: now}) // fail
	s.Close()

	success, _ := os.ReadFile(sp)
	fail, _ := os.ReadFile(fp)

	if n := strings.Count(string(success), "\n"); n != 2 {
		t.Errorf("success.log: want 2 lines, got %d:\n%s", n, success)
	}
	if n := strings.Count(string(fail), "\n"); n != 2 {
		t.Errorf("fail.log: want 2 lines, got %d:\n%s", n, fail)
	}
	if !strings.Contains(string(success), "/a") || !strings.Contains(string(success), "/b") {
		t.Error("success.log missing 2xx/3xx entries")
	}
	if !strings.Contains(string(fail), "/c") || !strings.Contains(string(fail), "/d") {
		t.Error("fail.log missing 4xx/5xx entries")
	}
	if strings.Contains(string(success), "/c") {
		t.Error("failure leaked into success.log")
	}
}
