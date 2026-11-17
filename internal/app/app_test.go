package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rdpmux/internal/assets"
	"rdpmux/internal/loc"
	"rdpmux/internal/logx"
	"rdpmux/internal/patcher"
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

func TestCheckPatchDBRequiresExactVersion(t *testing.T) {
	if _, err := checkPatchDB(assets.ConfigINI, "10.0.19041.7811"); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"10.0.19041.9999", "10.0.10240.19119"} {
		if _, err := checkPatchDB(assets.ConfigINI, version); err == nil || !strings.Contains(err.Error(), version) {
			t.Fatalf("invalid or unsupported version %s was accepted: %v", version, err)
		}
	}
	oldDB := bytes.ReplaceAll(assets.ConfigINI, []byte("10.0.19041.7811"), []byte("10.0.19041.7810"))
	if _, err := checkPatchDB(oldDB, "10.0.19041.7811"); err == nil {
		t.Fatal("outdated database was accepted")
	}
	if _, err := checkPatchDB([]byte("<html>error</html>"), "10.0.19041.7811"); err == nil {
		t.Fatal("invalid download was accepted")
	}
}

func TestWaitForPatchRequiresActiveMemory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	calls := 0
	if err := waitForPatch(ctx, func() (bool, error) {
		calls++
		return calls == 2, nil
	}); err != nil || calls != 2 {
		t.Fatalf("returned before patches became active: calls=%d err=%v", calls, err)
	}
}

func TestWaitForPatchReturnsVerificationErrors(t *testing.T) {
	failure := errors.New("cannot read TermService memory")
	if err := waitForPatch(context.Background(), func() (bool, error) {
		return false, failure
	}); !errors.Is(err, failure) {
		t.Fatalf("verification error lost: %v", err)
	}
}

func TestWaitForPatchCannotReportInactiveAsSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := waitForPatch(ctx, func() (bool, error) {
		calls++
		cancel()
		return false, nil
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("inactive patches were reported as successful: calls=%d err=%v", calls, err)
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

func TestReadInstallPatchDB(t *testing.T) {
	installDir, exeDir := t.TempDir(), t.TempDir()
	data, err := readInstallPatchDB(installDir, exeDir)
	if err != nil || !bytes.Equal(data, assets.ConfigINI) {
		t.Fatalf("expected embedded fallback when no INI exists: %v", err)
	}
	exeINI := []byte("[Main]\nUpdated=2026-11-16\n; from exe dir\n")
	if err := os.WriteFile(filepath.Join(exeDir, "rdpwrap.ini"), exeINI, 0644); err != nil {
		t.Fatal(err)
	}
	data, err = readInstallPatchDB(installDir, exeDir)
	if err != nil || !bytes.Equal(data, exeINI) {
		t.Fatalf("expected freshly updated exe-dir INI to be used: %v", err)
	}
	installINI := []byte("[Main]\nUpdated=2026-11-16\n; from install dir\n")
	if err := os.WriteFile(filepath.Join(installDir, "rdpwrap.ini"), installINI, 0644); err != nil {
		t.Fatal(err)
	}
	data, err = readInstallPatchDB(installDir, exeDir)
	if err != nil || !bytes.Equal(data, installINI) {
		t.Fatalf("expected existing install-dir INI to take priority: %v", err)
	}
}

func TestTermServiceRestartNeeded(t *testing.T) {
	const v = "10.0.19041.7811"
	newDB, err := patcher.LoadDB(assets.ConfigINI, "x64")
	if err != nil {
		t.Fatal(err)
	}
	if termServiceRestartNeeded(assets.ConfigINI, newDB, v, "x64") {
		t.Fatal("unchanged database requested a TermService restart")
	}
	newDB.Specs[v].LocalOnly.Offset++
	if !termServiceRestartNeeded(assets.ConfigINI, newDB, v, "x64") {
		t.Fatal("changed patch offset did not request a TermService restart")
	}
	newDB.Specs[v].LocalOnly.Offset--
	newDB.Specs[v].SLInitGlobals["bServerSku"] += 4
	if !termServiceRestartNeeded(assets.ConfigINI, newDB, v, "x64") {
		t.Fatal("changed SLInit global did not request a TermService restart")
	}
	newDB.Specs[v].SLInitGlobals["bServerSku"] -= 4
	// A change to a different version must not force a restart for this one.
	newDB.Specs["10.0.19041.7725"].LocalOnly.Offset++
	if termServiceRestartNeeded(assets.ConfigINI, newDB, v, "x64") {
		t.Fatal("unrelated version change requested a restart for this version")
	}
	if termServiceRestartNeeded(assets.ConfigINI, newDB, "10.0.99999.1", "x64") {
		t.Fatal("version absent from the old database requested a restart")
	}
	if termServiceRestartNeeded([]byte("not an ini"), newDB, v, "x64") {
		t.Fatal("unreadable old database requested a restart")
	}
}
