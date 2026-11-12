package gui

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"rdpmux/internal/app"
	"rdpmux/internal/sysconfig"
)

type fakeBackend struct {
	status func() app.Status
	write  func(sysconfig.Settings) error
}

func (b fakeBackend) Status() app.Status                       { return b.status() }
func (b fakeBackend) WriteSettings(s sysconfig.Settings) error { return b.write(s) }

type fakeSettings struct {
	value        sysconfig.Settings
	snapshots    int
	busy         bool
	applyEnabled bool
	reports      []error
	closes       int
}

func (v *fakeSettings) snapshotSettings() sysconfig.Settings {
	v.snapshots++
	return v.value
}
func (v *fakeSettings) setBusy(busy, dirty bool) {
	v.busy, v.applyEnabled = busy, !busy && dirty
}
func (v *fakeSettings) reportSave(err error) { v.reports = append(v.reports, err) }
func (v *fakeSettings) close()               { v.closes++ }

func testUI(t *testing.T, backend fakeBackend) (*ui, chan func(), *fakeSettings) {
	t.Helper()
	queue := make(chan func(), 32)
	view := &fakeSettings{value: sysconfig.Settings{Port: 3391, NLA: -1, Shadow: -1}}
	u := &ui{
		stopCh: make(chan struct{}), backend: backend, settings: view,
		enqueue: func(f func()) { queue <- f }, oldPort: 3389, dirty: true,
	}
	t.Cleanup(u.stop)
	return u, queue, view
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for test synchronization")
		var zero T
		return zero
	}
}

func waitWorker(t *testing.T, done <-chan struct{}) {
	t.Helper()
	if done != nil {
		receive(t, done)
	}
}

func unblock(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func assertEmpty[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected test event")
	default:
	}
}

func TestRebuiltWindowWaitsForOldQueryWithoutTick(t *testing.T) {
	oldEntered, newEntered := make(chan struct{}, 1), make(chan struct{}, 1)
	release := make(chan struct{})
	var active atomic.Int32
	enter := func() {
		if active.Add(1) != 1 {
			t.Error("status queries overlapped across windows")
		}
	}
	old, oldQueue, _ := testUI(t, fakeBackend{status: func() app.Status {
		enter()
		defer active.Add(-1)
		oldEntered <- struct{}{}
		<-release
		return app.Status{}
	}})
	canceled, canceledQueue, _ := testUI(t, fakeBackend{status: func() app.Status {
		t.Error("closed waiting window queried the system")
		return app.Status{}
	}})
	current, currentQueue, _ := testUI(t, fakeBackend{status: func() app.Status {
		enter()
		defer active.Add(-1)
		newEntered <- struct{}{}
		return app.Status{TermsrvVersion: "fresh"}
	}})
	var oldDone, canceledDone, currentDone <-chan struct{}
	defer func() {
		old.stop()
		canceled.stop()
		current.stop()
		unblock(release)
		waitWorker(t, oldDone)
		waitWorker(t, canceledDone)
		waitWorker(t, currentDone)
	}()
	oldDone = old.refreshStatusAsync()
	receive(t, oldEntered)
	old.stop()
	canceledDone = canceled.refreshStatusAsync()
	canceled.stop()
	waitWorker(t, canceledDone)
	if canceled.statusPending.Load() {
		t.Fatal("closed waiter remained pending")
	}
	currentDone = current.refreshStatusAsync()
	if currentDone == nil || !current.statusPending.Load() {
		t.Fatal("new window's initial refresh was dropped")
	}
	assertEmpty(t, newEntered)
	unblock(release)
	receive(t, newEntered)
	waitWorker(t, oldDone)
	waitWorker(t, currentDone)
	assertEmpty(t, oldQueue)
	assertEmpty(t, canceledQueue)
	if active.Load() != 0 || len(statusQueryGate) != 0 || current.statusPending.Load() {
		t.Fatal("completed queries retained ownership")
	}
	current.stop()
	receive(t, currentQueue)()
}

func TestClosedWaiterDoesNotQueryWhenGateBecomesReady(t *testing.T) {
	statusQueryGate <- struct{}{}
	held := true
	u, queue, _ := testUI(t, fakeBackend{status: func() app.Status {
		t.Error("stopped window started a query")
		return app.Status{}
	}})
	var done <-chan struct{}
	defer func() {
		u.stop()
		if held {
			<-statusQueryGate
		}
		waitWorker(t, done)
	}()
	done = u.refreshStatusAsync()
	u.stop()
	<-statusQueryGate
	held = false
	waitWorker(t, done)
	assertEmpty(t, queue)
	if u.statusPending.Load() || len(statusQueryGate) != 0 {
		t.Fatal("closed waiter retained query state")
	}
}

func TestPostDropsCallbackClosedAfterEnqueue(t *testing.T) {
	u, queue, _ := testUI(t, fakeBackend{})
	called := false
	u.post(func() { called = true })
	u.stop()
	receive(t, queue)()
	if called {
		t.Fatal("callback ran after window closure")
	}
}

func TestSlowSaveReturnsWithoutBlockingAndUsesSnapshot(t *testing.T) {
	entered := make(chan sysconfig.Settings, 2)
	release := make(chan struct{})
	u, queue, view := testUI(t, fakeBackend{write: func(s sysconfig.Settings) error {
		entered <- s
		<-release
		return nil
	}})
	returned := make(chan (<-chan struct{}), 1)
	var done <-chan struct{}
	defer func() {
		u.stop()
		unblock(release)
		if done == nil {
			done = receive(t, returned)
		}
		waitWorker(t, done)
	}()
	go func() { returned <- u.writeSettings(false) }()
	done = receive(t, returned)
	if done == nil {
		t.Fatal("save did not start")
	}
	saved := receive(t, entered)
	if saved != view.value || !u.opBusy || !view.busy {
		t.Fatal("save did not capture settings and enter busy state")
	}
	view.value.Port = 4444
	u.onApply()
	u.onOK()
	u.onCancel()
	u.onLanguage()
	canceled := false
	u.onClosing(&canceled, 0)
	if !canceled || u.stopped() || view.closes != 0 || view.snapshots != 1 {
		t.Fatal("busy save accepted another operation or close")
	}
	assertEmpty(t, entered)
	if u.oldPort != 3389 || !u.dirty || len(view.reports) != 0 {
		t.Fatal("save updated state before completion")
	}
	unblock(release)
	waitWorker(t, done)
	if !u.opBusy || u.oldPort != 3389 || !u.dirty || len(view.reports) != 0 {
		t.Fatal("worker changed UI state before its callback")
	}
	receive(t, queue)()
	if u.opBusy || view.busy || u.dirty || u.oldPort != saved.Port || saved.Port != 3391 || view.applyEnabled {
		t.Fatal("save did not commit its original snapshot on the UI callback")
	}
	if len(view.reports) != 1 || view.reports[0] != nil || view.closes != 0 {
		t.Fatal("Apply completion reported or closed incorrectly")
	}
}

func TestSaveCompletionPreservesFailureAndClosesOnlySuccessfulOK(t *testing.T) {
	failure := errors.New("injected save failure")
	for _, tt := range []struct {
		name           string
		closeOnSuccess bool
		err            error
	}{
		{"Apply success", false, nil},
		{"OK success", true, nil},
		{"Apply failure", false, failure},
		{"OK failure", true, failure},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u, queue, view := testUI(t, fakeBackend{write: func(sysconfig.Settings) error { return tt.err }})
			done := u.writeSettings(tt.closeOnSuccess)
			defer waitWorker(t, done)
			waitWorker(t, done)
			if !u.opBusy || !u.dirty || u.oldPort != 3389 || view.closes != 0 || len(view.reports) != 0 {
				t.Fatal("worker changed UI state directly")
			}
			receive(t, queue)()
			if u.opBusy || view.busy || len(view.reports) != 1 || !errors.Is(view.reports[0], tt.err) {
				t.Fatal("save completion was not reported correctly")
			}
			if tt.err != nil {
				if !u.dirty || u.oldPort != 3389 || !view.applyEnabled || view.closes != 0 {
					t.Fatal("failed save changed committed state or closed the window")
				}
			} else if u.dirty || u.oldPort != 3391 || view.applyEnabled || (view.closes == 1) != tt.closeOnSuccess {
				t.Fatal("successful save did not commit or close correctly")
			}
		})
	}
}

func TestCleanOKClosesWithoutSaving(t *testing.T) {
	u, _, view := testUI(t, fakeBackend{write: func(sysconfig.Settings) error {
		t.Error("clean OK wrote settings")
		return nil
	}})
	u.dirty = false
	u.onOK()
	if view.closes != 1 || view.snapshots != 0 || u.opBusy {
		t.Fatal("clean OK did not close without saving")
	}
}

func TestDestroyedWindowDropsSaveCompletion(t *testing.T) {
	for _, afterEnqueue := range []bool{false, true} {
		t.Run(map[bool]string{false: "during save", true: "after enqueue"}[afterEnqueue], func(t *testing.T) {
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			u, queue, view := testUI(t, fakeBackend{write: func(sysconfig.Settings) error {
				entered <- struct{}{}
				<-release
				return nil
			}})
			done := u.writeSettings(true)
			defer func() { u.stop(); unblock(release); waitWorker(t, done) }()
			receive(t, entered)
			if afterEnqueue {
				unblock(release)
				waitWorker(t, done)
				u.stop()
				receive(t, queue)()
			} else {
				u.stop()
				unblock(release)
				waitWorker(t, done)
				assertEmpty(t, queue)
			}
			if !u.dirty || u.oldPort != 3389 || len(view.reports) != 0 || view.closes != 0 {
				t.Fatal("disposed window received save completion")
			}
		})
	}
}
