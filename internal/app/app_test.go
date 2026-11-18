package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rdpmux/internal/assets"
	"rdpmux/internal/loc"
	"rdpmux/internal/logx"
	"rdpmux/internal/sysconfig"
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

func TestListenerOnlyError(t *testing.T) {
	listener := fmt.Errorf("TermService is running but the %w", sysconfig.ErrListenerNotReady)
	other := errors.New("stop UmRdpService: access denied")
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"listener", listener, true},
		{"joined listener", errors.Join(listener), true},
		{"two listeners", errors.Join(listener, listener), true},
		{"listener and stop failure", errors.Join(listener, other), false},
		{"unrelated failure", other, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := listenerOnlyError(tt.err); got != tt.want {
				t.Fatalf("listenerOnlyError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestOperationResult(t *testing.T) {
	for _, tt := range []struct {
		name    string
		data    string
		wantErr bool
	}{
		{"ok", "ok\n", false},
		{"ok without newline", "ok", false},
		{"failure", "error: stop TermService: timeout\n", true},
		{"empty", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := operationResult(tt.data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("operationResult(%q) = %v, want error %v", tt.data, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), strings.TrimPrefix(strings.TrimSpace(tt.data), "error: ")) {
				t.Fatalf("result text lost: %v", err)
			}
		})
	}
}

func TestFollowOperationStreamsLogAndResult(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "op.log")
	resultPath := filepath.Join(dir, "op.result")
	if err := os.WriteFile(logPath, []byte("line one\r\nline two\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, []byte("ok\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	complete, err := followOperation(&out, logPath, resultPath, 5*time.Second)
	if err != nil || !complete {
		t.Fatalf("complete=%v err=%v", complete, err)
	}
	if out.String() != "line one\r\nline two\r\n" {
		t.Fatalf("streamed log = %q", out.String())
	}
}

func TestFollowOperationReportsHostFailure(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "op.log")
	resultPath := filepath.Join(dir, "op.result")
	if err := os.WriteFile(logPath, []byte("started\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, []byte("error: stop TermService: timeout\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	complete, err := followOperation(&out, logPath, resultPath, 5*time.Second)
	if err == nil || err.Error() != "stop TermService: timeout" || !complete {
		t.Fatalf("complete=%v error=%v", complete, err)
	}
	if out.String() != "started\r\n" {
		t.Fatalf("streamed log = %q", out.String())
	}
}

func TestFollowOperationTimesOutWithoutResult(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "op.log")
	if err := os.WriteFile(logPath, []byte("started\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	complete, err := followOperation(&out, logPath, filepath.Join(dir, "missing.result"), 150*time.Millisecond)
	if err == nil || complete {
		t.Fatalf("complete=%v error=%v", complete, err)
	}
	if !strings.Contains(err.Error(), logPath) {
		t.Fatalf("error does not point at the retained log: %v", err)
	}
	if out.String() != "started\r\n" {
		t.Fatalf("streamed log = %q", out.String())
	}
}

func TestExecuteOperationRejectsUnknown(t *testing.T) {
	if err := executeOperation("no-such-operation", logx.Discard); err == nil {
		t.Fatal("unknown operation was accepted")
	}
}

func TestServeOperationHost(t *testing.T) {
	for _, tt := range []struct {
		name        string
		args        []string
		wantHandled bool
	}{
		{"no args", nil, false},
		{"normal command", []string{"install"}, false},
		{"host entry", []string{OperationCommand}, true},
		{"host entry with args", []string{OperationCommand, "install"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, err := ServeOperationHost(tt.args)
			if handled != tt.wantHandled {
				t.Fatalf("handled = %v, want %v", handled, tt.wantHandled)
			}
			if handled && err == nil {
				t.Fatalf("invalid host arguments were accepted")
			}
		})
	}
}

func TestRunOperationHostRequiresArguments(t *testing.T) {
	if err := RunOperationHost(nil); err == nil {
		t.Fatal("missing arguments were accepted")
	}
	if err := RunOperationHost([]string{"install", "info", "log"}); err == nil {
		t.Fatal("incomplete arguments were accepted")
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
