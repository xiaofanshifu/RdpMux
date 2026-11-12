package logx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func newTestWriter(t *testing.T, path string, limit int64) *RotatingFile {
	t.Helper()
	w, err := NewRotatingFile(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w
}

func requireFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func TestTwoWritersRotateSamePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.log")
	writers := []*RotatingFile{newTestWriter(t, path, 8), newTestWriter(t, strings.ToUpper(path), 8)}
	for i, text := range []string{"1111", "2222", "3333", "4444", "5555"} {
		if n, err := io.WriteString(writers[i%2], text); err != nil || n != len(text) {
			t.Fatalf("write %d: n=%d, err=%v", i, n, err)
		}
	}
	requireFile(t, path+".1", "33334444")
	requireFile(t, path, "5555")
	// Even live writers must not keep the target locked between writes.
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writers[0], "6666"); err != nil {
		t.Fatal(err)
	}
	requireFile(t, path, "6666")
}

func TestTwoWritersRotateConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.log")
	writers := []*RotatingFile{newTestWriter(t, path, 64), newTestWriter(t, strings.ToUpper(path), 64)}
	var wg sync.WaitGroup
	for i, w := range writers {
		wg.Add(1)
		go func(i int, w *RotatingFile) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				line := fmt.Sprintf("%d:%04d\n", i, j)
				if n, err := io.WriteString(w, line); err != nil || n != len(line) {
					t.Errorf("write: n=%d, err=%v", n, err)
					return
				}
			}
		}(i, w)
	}
	wg.Wait()
	checkRotatedRecords(t, path, 64)
}

func checkRotatedRecords(t *testing.T, path string, limit int) {
	t.Helper()
	for _, p := range []string{path, path + ".1"} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 || len(data) > limit || len(data)%7 != 0 {
			t.Fatalf("invalid rotated size: %s = %d", p, len(data))
		}
		for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
			var writer, record int
			if _, err := fmt.Sscanf(string(line), "%d:%04d", &writer, &record); err != nil || writer < 0 || writer > 1 || record < 0 || record >= 100 {
				t.Fatalf("corrupt record %q", line)
			}
		}
	}
}

func TestRotationFailureIsReturned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.log")
	w := newTestWriter(t, path, 4)
	if _, err := io.WriteString(w, "1111"); err != nil {
		t.Fatal(err)
	}
	// A non-empty backup directory prevents replacement without changing ACLs.
	if err := os.Mkdir(path+".1", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".1", "keep"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if n, err := io.WriteString(w, "2222"); err == nil || n != 0 {
		t.Fatalf("rotation failure was ignored: n=%d, err=%v", n, err)
	}
	requireFile(t, path, "1111")
}

func TestRotatingFileCloseIsFinal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.log")
	w := newTestWriter(t, path, 4)
	w.Close()
	w.Close()
	if _, err := w.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Write after Close = %v", err)
	}
	newTestWriter(t, path, 4)
}

func logHelper(t *testing.T, path, mode string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestRotatingFileProcessHelper$")
	cmd.Env = append(os.Environ(), "RDPMUX_LOG_TEST_PATH="+path, "RDPMUX_LOG_TEST_MODE="+mode)
	return cmd
}

func TestRotatingFileAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.log")
	cmds := []*exec.Cmd{logHelper(t, path, "0"), logHelper(t, strings.ToUpper(path), "1")}
	outputs := make([]bytes.Buffer, len(cmds))
	for i, cmd := range cmds {
		cmd.Stdout, cmd.Stderr = &outputs[i], &outputs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper: %v: %s", err, outputs[i].String())
		}
	}
	checkRotatedRecords(t, path, 64)
}

func TestRotatingFileAbandonedMutex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.log")
	w := newTestWriter(t, path, 64)
	if out, err := logHelper(t, path, "abandon").CombinedOutput(); err != nil {
		t.Fatalf("helper: %v: %s", err, out)
	}
	if _, err := io.WriteString(w, "recovered"); err != nil {
		t.Fatalf("abandoned mutex was not recovered: %v", err)
	}
	requireFile(t, path, "recovered")
}

func TestRotatingFileProcessHelper(t *testing.T) {
	path := os.Getenv("RDPMUX_LOG_TEST_PATH")
	if path == "" {
		return
	}
	w, err := NewRotatingFile(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	mode := os.Getenv("RDPMUX_LOG_TEST_MODE")
	if mode == "abandon" {
		runtime.LockOSThread()
		result, err := windows.WaitForSingleObject(w.mutex, rotationLockTimeout)
		if err != nil || result != windows.WAIT_OBJECT_0 {
			t.Fatalf("acquire mutex: %d, %v", result, err)
		}
		// Only this isolated test subprocess exits, leaving an abandoned lock.
		os.Exit(0)
	}
	for i := 0; i < 100; i++ {
		if _, err := fmt.Fprintf(w, "%s:%04d\n", mode, i); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRotatingFileMutexTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.log")
	owner := newTestWriter(t, path, 64)
	other := newTestWriter(t, strings.ToUpper(path), 64)
	ready := make(chan error, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		result, err := windows.WaitForSingleObject(owner.mutex, rotationLockTimeout)
		if err != nil || result != windows.WAIT_OBJECT_0 {
			ready <- fmt.Errorf("acquire mutex: %d, %v", result, err)
			return
		}
		defer windows.ReleaseMutex(owner.mutex)
		ready <- nil
		<-release
	}()
	defer func() { close(release); <-done }()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	if _, err := other.Write([]byte("blocked")); !errors.Is(err, windows.WAIT_TIMEOUT) {
		t.Fatalf("expected timeout, got %v", err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestLoggerRetainsWriterErrorAndContinues(t *testing.T) {
	for _, failure := range []error{errors.New("writer failed"), nil} {
		var output bytes.Buffer
		log := New(Info, failingWriter{failure}, &output)
		log.Info("first")
		log.Info("second")
		want := failure
		if want == nil {
			want = io.ErrShortWrite
		}
		if !errors.Is(log.Err(), want) {
			t.Fatalf("Err = %v, want %v", log.Err(), want)
		}
		if !strings.Contains(output.String(), "first") || !strings.Contains(output.String(), "second") {
			t.Fatal("healthy destination stopped receiving logs")
		}
	}
}
