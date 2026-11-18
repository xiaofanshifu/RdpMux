package winsvc

import (
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func lockImageForTest(t *testing.T, path string) windows.Handle {
	t.Helper()
	h, err := lockImage(path)
	if err != nil {
		t.Fatalf("lock %s: %v", path, err)
	}
	return h
}

func TestLockImageAcceptsRunningImage(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := lockImageForTest(t, exe)
	windows.CloseHandle(h)
}

func TestLockImageAllowsExecution(t *testing.T) {
	target := `C:\Windows\System32\whoami.exe`
	h := lockImageForTest(t, target)
	defer windows.CloseHandle(h)
	out, err := exec.Command(target).CombinedOutput()
	if err != nil {
		t.Fatalf("execute guarded image: %v (%s)", err, out)
	}
	t.Logf("guarded image ran: %s", out)
}

func TestLockImageBlocksReplacement(t *testing.T) {
	dir := t.TempDir()
	src := dir + `\a.exe`
	if err := os.WriteFile(src, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	h := lockImageForTest(t, src)
	defer windows.CloseHandle(h)
	if err := os.Rename(src, dir+`\b.exe`); err == nil {
		t.Fatal("rename succeeded while the image was guarded")
	}
}
