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
