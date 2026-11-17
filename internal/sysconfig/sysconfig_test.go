package sysconfig

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRollbackAggregatesErrors(t *testing.T) {
	primary, restoreFailure := errors.New("write failed"), errors.New("restore failed")
	var order []int
	changes := &rdpChanges{undo: []func() error{
		func() error { order = append(order, 1); return nil },
		func() error {
			order = append(order, 2)
			return restoreFailure
		},
		func() error { order = append(order, 3); return nil },
	}}
	err := changes.fail(primary)
	if !errors.Is(err, primary) || !errors.Is(err, restoreFailure) || !reflect.DeepEqual(order, []int{3, 2, 1}) {
		t.Fatalf("rollback = %v, %v", order, err)
	}
}

func fakeTermServiceOps(events *[]string) termServiceOps {
	return termServiceOps{
		find: func() (uint32, []string, error) { return 1, nil, nil },
		dependents: func(name string) ([]string, error) {
			switch strings.ToLower(name) {
			case "termservice":
				return []string{"dependent"}, nil
			case "dependent":
				return []string{"child"}, nil
			default:
				return nil, nil
			}
		},
		query: func(string) (uint32, uint32, error) { return windows.SERVICE_RUNNING, 1, nil },
		stop: func(name string) error {
			*events = append(*events, "stop "+name)
			return nil
		},
		start: func(name string) error {
			*events = append(*events, "start "+name)
			return nil
		},
	}
}

func TestRestartTermServiceDependencyOrder(t *testing.T) {
	var events []string
	ops := fakeTermServiceOps(&events)
	ops.find = func() (uint32, []string, error) { return 1, []string{"dependent", "DEPENDENT", "idle"}, nil }
	ops.query = func(name string) (uint32, uint32, error) {
		if name == "idle" {
			return windows.SERVICE_STOPPED, 0, nil
		}
		return windows.SERVICE_RUNNING, 1, nil
	}
	if err := restartTermService(ops); err != nil {
		t.Fatal(err)
	}
	want := []string{"stop child", "stop dependent", "stop TermService", "start TermService", "start dependent", "start child"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRestartTermServicePreflightErrorsDoNotStopServices(t *testing.T) {
	failure := errors.New("enumeration/query failed")
	for _, stage := range []string{"find", "dependents", "query"} {
		t.Run(stage, func(t *testing.T) {
			var events []string
			ops := fakeTermServiceOps(&events)
			switch stage {
			case "find":
				ops.find = func() (uint32, []string, error) { return 0, nil, failure }
			case "dependents":
				ops.dependents = func(string) ([]string, error) { return nil, failure }
			case "query":
				ops.query = func(string) (uint32, uint32, error) { return 0, 0, failure }
			}
			err := restartTermService(ops)
			if err == nil || !errors.Is(err, failure) || len(events) != 0 {
				t.Fatalf("events = %v, error = %v", events, err)
			}
		})
	}
}

func TestRestartTermServiceStopFailureRestoresAndJoinsErrors(t *testing.T) {
	stopFailure, startFailure := errors.New("stop failed"), errors.New("restore failed")
	var events []string
	ops := fakeTermServiceOps(&events)
	ops.stop = func(name string) error {
		events = append(events, "stop "+name)
		if name == "dependent" {
			return stopFailure
		}
		return nil
	}
	ops.start = func(name string) error {
		events = append(events, "start "+name)
		if name == "dependent" {
			return startFailure
		}
		return nil
	}
	err := restartTermService(ops)
	want := []string{"stop child", "stop dependent", "start dependent", "start child"}
	if !errors.Is(err, stopFailure) || !errors.Is(err, startFailure) || !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, error = %v", events, err)
	}
}

func TestRestartTermServiceStartsStoppedTarget(t *testing.T) {
	var events []string
	ops := fakeTermServiceOps(&events)
	ops.query = func(string) (uint32, uint32, error) { return windows.SERVICE_STOPPED, 0, nil }
	if err := restartTermService(ops); err != nil {
		t.Fatal(err)
	}
	want := []string{"stop TermService", "start TermService"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}
