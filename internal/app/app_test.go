package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rdpmux/internal/loc"
	"rdpmux/internal/logx"
)

func TestNewLoggerReportsFileFailureWithoutConsole(t *testing.T) {
	var diagnostic bytes.Buffer
	log, closeLog := newLogger(logx.Info, nil, t.TempDir(), &diagnostic)
	defer closeLog()
	log.Info("status remains available")
	if !strings.Contains(diagnostic.String(), "cannot open log file:") {
		t.Fatalf("missing file failure diagnostic: %q", diagnostic.String())
	}
}

func TestNewLoggerPreservesConsoleWhenFileFails(t *testing.T) {
	var console, diagnostic bytes.Buffer
	log, closeLog := newLogger(logx.Info, &console, t.TempDir(), &diagnostic)
	defer closeLog()
	log.Info("status remains available")
	if !strings.Contains(console.String(), "cannot open log file:") || !strings.Contains(console.String(), "status remains available") {
		t.Fatalf("console output lost: %q", console.String())
	}
	if diagnostic.Len() != 0 {
		t.Fatalf("duplicate diagnostic: %q", diagnostic.String())
	}
}

func TestNewLoggerWritesFileWithoutConsole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rdpmux.log")
	var diagnostic bytes.Buffer
	log, closeLog := newLogger(logx.Info, nil, path, &diagnostic)
	log.Info("status recorded")
	closeLog()
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "status recorded") {
		t.Fatalf("log output missing: %q, %v", data, err)
	}
	if diagnostic.Len() != 0 {
		t.Fatalf("unexpected diagnostic: %q", diagnostic.String())
	}
}

func TestNewLoggerLocalizesFileFailure(t *testing.T) {
	old := loc.CurrentLanguage()
	t.Cleanup(func() { loc.SetCurrent(old, false) })
	if !loc.SetCurrent("zh-CN", false) {
		t.Fatal("failed to load Chinese")
	}
	var diagnostic bytes.Buffer
	_, closeLog := newLogger(logx.Info, nil, t.TempDir(), &diagnostic)
	defer closeLog()
	if !strings.Contains(diagnostic.String(), "无法打开日志文件") {
		t.Fatalf("untranslated diagnostic: %q", diagnostic.String())
	}
}

func TestWriteFileAtomicReplacesContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rdpwrap.ini")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("%q %v", data, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("temporary files remain: %v %v", files, err)
	}
}

func TestWriteFileAtomicPreservesLockedDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rdpwrap.ini")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := writeFileAtomic(path, []byte("new")); err == nil {
		t.Fatal("expected locked file replacement to fail")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "old" {
		t.Fatalf("old database lost: %q %v", data, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("temporary files remain: %v %v", files, err)
	}
}

func TestPendingDeletion(t *testing.T) {
	dir := `C:\Program Files\RdpMux`
	for _, tt := range []struct {
		entries []string
		want    bool
	}{
		{[]string{`\??\C:\Program Files\RdpMux\rdpmux.exe`, ""}, true},
		{[]string{`\??\C:\Program Files\RdpMux\rdpmux.exe`, `\??\C:\backup.exe`}, false},
		{[]string{`\??\C:\Other\rdpmux.exe`, ""}, false},
		{nil, false},
	} {
		if got := pendingDeletion(tt.entries, dir); got != tt.want {
			t.Errorf("%v: %v want %v", tt.entries, got, tt.want)
		}
	}
}
