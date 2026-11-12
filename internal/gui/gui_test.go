package gui

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"rdpmux/internal/app"
	"rdpmux/internal/logx"
)

// These tests deliberately use no Walk windows and perform no system actions.
func TestMstscArgsUseSavedPort(t *testing.T) {
	for _, port := range []uint32{3389, 3391} {
		got := mstscArgs(port, "/w:800 /h:600")
		endpoint := "/v:127.0.0.2:3389"
		if port == 3391 {
			endpoint = "/v:127.0.0.2:3391"
		}
		want := []string{endpoint, "/w:800", "/h:600", "/prompt"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mstscArgs(%d) = %v, want %v", port, got, want)
		}
	}
}

func TestBusyOrClosedUIRejectsOperations(t *testing.T) {
	for _, closed := range []bool{false, true} {
		u := &ui{opBusy: !closed, dirty: true, oldPort: 3391, stopCh: make(chan struct{})}
		if closed {
			u.stop()
		}
		// Any access to controls, registry or the operation body would fail.
		u.runOp("must not run", func(*logx.Logger) error {
			t.Error("busy/closed UI started an operation")
			return nil
		})
		if u.writeSettings(false) != nil {
			t.Fatal("busy/closed UI started writing settings")
		}
		u.onApply()
		u.onOK()
		u.onCancel()
		u.onUninstall()
		u.onLanguage()
		u.mstsc("/f")
		if !u.dirty || u.oldPort != 3391 {
			t.Fatal("blocked operation changed unsaved state")
		}
	}
}

func TestBusyCloseIsCanceled(t *testing.T) {
	u := &ui{opBusy: true, stopCh: make(chan struct{})}
	canceled := false
	u.onClosing(&canceled, 0)
	if !canceled || u.stopped() {
		t.Fatal("busy close was accepted")
	}
	u.opBusy = false
	canceled = false
	u.onClosing(&canceled, 0)
	if canceled || !u.stopped() {
		t.Fatal("idle close did not stop callbacks")
	}
}

func TestClosedUIIgnoresCallbacks(t *testing.T) {
	u := &ui{stopCh: make(chan struct{})}
	u.stop()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u.stop()
			u.post(func() { t.Error("callback ran on closed UI") })
			if _, err := (guiWriter{u}).Write([]byte("ignored")); err != nil {
				t.Error(err)
			}
			u.refreshStatusAsync()
		}()
	}
	wg.Wait()
	done := make(chan struct{})
	go func() { u.tick(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("polling did not stop")
	}
}

func TestStatusRefreshSkipsOverlappingQuery(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	u, queue, _ := testUI(t, fakeBackend{status: func() app.Status {
		entered <- struct{}{}
		<-release
		return app.Status{}
	}})
	var first, next <-chan struct{}
	defer func() {
		u.stop()
		unblock(release)
		waitWorker(t, first)
		waitWorker(t, next)
	}()
	first = u.refreshStatusAsync()
	receive(t, entered)
	for range 8 {
		if u.refreshStatusAsync() != nil {
			t.Fatal("overlapping refresh was not merged")
		}
	}
	if !u.statusPending.Load() {
		t.Fatal("refresh disturbed an existing query")
	}
	unblock(release)
	waitWorker(t, first)
	assertEmpty(t, entered)
	if u.statusPending.Load() {
		t.Fatal("finished query remained pending")
	}
	next = u.refreshStatusAsync()
	if next == nil {
		t.Fatal("finished query prevented another refresh")
	}
	receive(t, entered)
	waitWorker(t, next)
	u.stop()
	receive(t, queue)()
	receive(t, queue)()
}
