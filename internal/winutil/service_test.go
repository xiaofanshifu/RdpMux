package winutil

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestEnumServicePagesPreservesPagesAndResume(t *testing.T) {
	calls := 0
	got, err := enumServicePages(func(resume *uint32) ([]ServiceInfo, error) {
		if *resume != uint32(calls) {
			t.Fatalf("resume = %d, want %d", *resume, calls)
		}
		calls++
		*resume = uint32(calls)
		page := []ServiceInfo{{Name: string(rune('A' + calls - 1)), PID: uint32(calls)}}
		if calls < 3 {
			return page, windows.ERROR_MORE_DATA
		}
		return page, nil
	})
	want := []ServiceInfo{{Name: "A", PID: 1}, {Name: "B", PID: 2}, {Name: "C", PID: 3}}
	if err != nil || !reflect.DeepEqual(got, want) || calls != 3 {
		t.Fatalf("got %v, %v (%d calls), want %v", got, err, calls, want)
	}
}

func TestEnumServicePagesRejectsErrorsAndNoProgress(t *testing.T) {
	for _, tc := range []struct {
		name string
		next func(*uint32) ([]ServiceInfo, error)
		want error
	}{
		{"query failure", func(*uint32) ([]ServiceInfo, error) { return nil, windows.ERROR_ACCESS_DENIED }, windows.ERROR_ACCESS_DENIED},
		{"unchanged resume", func(*uint32) ([]ServiceInfo, error) {
			return []ServiceInfo{{Name: "A"}}, windows.ERROR_MORE_DATA
		}, windows.ERROR_MORE_DATA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := enumServicePages(tc.next); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEnumServicePagesAllowsEmptyPageWithProgress(t *testing.T) {
	calls := 0
	got, err := enumServicePages(func(resume *uint32) ([]ServiceInfo, error) {
		calls++
		if calls == 1 {
			*resume = 1
			return nil, windows.ERROR_MORE_DATA
		}
		return []ServiceInfo{{Name: "visible", PID: 1}}, nil
	})
	if err != nil || len(got) != 1 || calls != 2 {
		t.Fatalf("%v %v calls=%d", got, err, calls)
	}
}

func TestWaitServiceState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target uint32
		state  uint32
		query  error
		want   error
	}{
		{"already running", windows.SERVICE_RUNNING, windows.SERVICE_RUNNING, nil, nil},
		{"already stopped", windows.SERVICE_STOPPED, windows.SERVICE_STOPPED, nil, nil},
		{"start timeout", windows.SERVICE_RUNNING, windows.SERVICE_START_PENDING, nil, windows.ERROR_SERVICE_REQUEST_TIMEOUT},
		{"stop timeout", windows.SERVICE_STOPPED, windows.SERVICE_STOP_PENDING, nil, windows.ERROR_SERVICE_REQUEST_TIMEOUT},
		{"query error", windows.SERVICE_STOPPED, 0, windows.ERROR_ACCESS_DENIED, windows.ERROR_ACCESS_DENIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := waitServiceState("test", tc.target, func() (uint32, error) { return tc.state, tc.query }, 0)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	if err := waitServiceState("test", windows.SERVICE_RUNNING, func() (uint32, error) {
		return windows.SERVICE_STOPPED, nil
	}, time.Second); err == nil {
		t.Fatal("a service that stopped during startup was reported as running")
	}
}

func TestWaitServiceStateWaitsForRunning(t *testing.T) {
	calls := 0
	err := waitServiceState("test", windows.SERVICE_RUNNING, func() (uint32, error) {
		calls++
		if calls == 1 {
			return windows.SERVICE_START_PENDING, nil
		}
		return windows.SERVICE_RUNNING, nil
	}, time.Second)
	if err != nil || calls != 2 {
		t.Fatalf("error = %v, queries = %d", err, calls)
	}
}

func TestWaitServiceStoppedDetectsReplacementByPID(t *testing.T) {
	calls := 0
	start := time.Now()
	err := waitServiceStopped("TermService", 5520, func() (ServiceStatus, error) {
		calls++
		if calls == 1 {
			return ServiceStatus{State: windows.SERVICE_STOP_PENDING, PID: 5520}, nil
		}
		return ServiceStatus{State: windows.SERVICE_RUNNING, PID: 1752}, nil
	}, 2*time.Second)
	if !errors.Is(err, ErrServiceReplaced) {
		t.Fatalf("error = %v, want ErrServiceReplaced", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("replacement was not reported promptly: %s", elapsed)
	}
}

func TestWaitServiceStoppedDetectsRestartWithoutPID(t *testing.T) {
	states := []uint32{windows.SERVICE_STOP_PENDING, windows.SERVICE_START_PENDING, windows.SERVICE_RUNNING}
	calls := 0
	err := waitServiceStopped("TermService", 0, func() (ServiceStatus, error) {
		state := states[len(states)-1]
		if calls < len(states) {
			state = states[calls]
		}
		calls++
		return ServiceStatus{State: state}, nil
	}, 2*time.Second)
	if !errors.Is(err, ErrServiceReplaced) {
		t.Fatalf("error = %v, want ErrServiceReplaced", err)
	}
}

func TestWaitServiceStoppedToleratesLateStopPending(t *testing.T) {
	seq := []ServiceStatus{
		{State: windows.SERVICE_RUNNING, PID: 5520},
		{State: windows.SERVICE_STOP_PENDING, PID: 5520},
		{State: windows.SERVICE_STOPPED},
	}
	calls := 0
	err := waitServiceStopped("TermService", 5520, func() (ServiceStatus, error) {
		st := seq[len(seq)-1]
		if calls < len(seq) {
			st = seq[calls]
		}
		calls++
		return st, nil
	}, 2*time.Second)
	if err != nil {
		t.Fatalf("a pending stop was reported as a restart: %v", err)
	}
}

func TestWaitServiceStoppedTimesOutWhenServiceHangs(t *testing.T) {
	err := waitServiceStopped("test", 1, func() (ServiceStatus, error) {
		return ServiceStatus{State: windows.SERVICE_STOP_PENDING, PID: 1}, nil
	}, 0)
	if !errors.Is(err, windows.ERROR_SERVICE_REQUEST_TIMEOUT) {
		t.Fatalf("error = %v, want timeout", err)
	}
}

// A service that keeps running with an unchanged pid never began stopping: the
// stop must be reported after the grace period instead of at the full timeout.
func TestWaitServiceStoppedReportsStopThatNeverStarts(t *testing.T) {
	start := time.Now()
	err := waitServiceStopped("TermService", 5520, func() (ServiceStatus, error) {
		return ServiceStatus{State: windows.SERVICE_RUNNING, PID: 5520}, nil
	}, 300*time.Millisecond)
	if !errors.Is(err, ErrStopNotStarted) {
		t.Fatalf("error = %v, want ErrStopNotStarted", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stop that never began took %s", elapsed)
	}
}
