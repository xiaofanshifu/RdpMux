package sysconfig

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func productRules(port string, enabled bool) []firewallRule {
	return []firewallRule{
		{Name: "RdpMux TCP", Description: firewallOwner, Protocol: 6, Direction: 1, LocalPorts: port, Enabled: enabled},
		{Name: "RdpMux UDP", Description: firewallOwner, Protocol: 17, Direction: 1, LocalPorts: port, Enabled: enabled},
	}
}

func TestFirewallCreationRollbackAndIdempotence(t *testing.T) {
	var calls []string
	run := func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	changes := &rdpChanges{}
	if err := changes.configureFirewall(3391, []firewallRule{{Name: "Remote Desktop"}}, run); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !strings.Contains(calls[0], "name=RdpMux TCP") || !strings.Contains(calls[1], "name=RdpMux UDP") {
		t.Fatalf("incorrect creation commands: %v", calls)
	}
	for _, call := range calls {
		if !strings.Contains(call, "localport=3391") || !strings.Contains(call, "description="+firewallOwner) || strings.Contains(call, "Remote Desktop") {
			t.Fatalf("incorrect rule scope or port: %s", call)
		}
	}
	if err := changes.rollback(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 || !strings.HasPrefix(calls[2], "delete rule name=RdpMux UDP") || !strings.HasPrefix(calls[3], "delete rule name=RdpMux TCP") {
		t.Fatalf("incorrect rollback: %v", calls)
	}
	if err := changes.rollback(); err != nil || len(calls) != 4 {
		t.Fatalf("rollback repeated successful steps: %v, %v", calls, err)
	}
	calls = nil
	if err := (&rdpChanges{}).configureFirewall(3391, productRules("3391", true), run); err != nil || len(calls) != 0 {
		t.Fatalf("existing correct rules changed: %v, %v", calls, err)
	}
}

func TestFirewallRestoresExistingPortAndEnabledState(t *testing.T) {
	var calls []string
	changes := &rdpChanges{}
	err := changes.configureFirewall(3392, productRules("4400-4402", false), func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := changes.rollback(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("calls = %v", calls)
	}
	for i, call := range calls {
		if !strings.HasPrefix(call, "set rule name=RdpMux ") {
			t.Fatalf("pre-existing rule was not updated in place: %s", call)
		}
		want := "new localport=3392 enable=yes"
		if i >= 2 {
			want = "new localport=4400-4402 enable=no"
		}
		if !strings.HasSuffix(call, want) {
			t.Fatalf("call = %s, want suffix %s", call, want)
		}
	}
}

func TestFirewallFailureRollsBackOnlyCreatedRules(t *testing.T) {
	failure := errors.New("UDP add failed")
	var calls []string
	changes := &rdpChanges{}
	err := changes.configureFirewall(3390, nil, func(args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if strings.HasPrefix(call, "add rule name=RdpMux UDP") {
			return failure
		}
		return nil
	})
	err = changes.fail(err)
	if !errors.Is(err, failure) || len(calls) != 3 || !strings.HasPrefix(calls[2], "delete rule name=RdpMux TCP") {
		t.Fatalf("failed add was swallowed or rollback incorrect: %v, %v", calls, err)
	}
}

func TestFirewallSetFailureRestoresBothRules(t *testing.T) {
	failure := errors.New("UDP update failed")
	var calls []string
	changes := &rdpChanges{}
	err := changes.configureFirewall(3390, productRules("3389", false), func(args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if strings.Contains(call, "name=RdpMux UDP") && strings.Contains(call, "new localport=3390") {
			return failure
		}
		return nil
	})
	err = changes.fail(err)
	if !errors.Is(err, failure) || len(calls) != 4 {
		t.Fatalf("error = %v, calls = %v", err, calls)
	}
	for _, call := range calls[2:] {
		if !strings.HasSuffix(call, "new localport=3389 enable=no") || !strings.HasPrefix(call, "set rule ") {
			t.Fatalf("pre-existing rule was not restored: %s", call)
		}
	}
}

func TestFirewallWildcardPortRestoration(t *testing.T) {
	var call string
	err := setFirewallPort(func(args ...string) error {
		call = strings.Join(args, " ")
		return nil
	}, productRules("*", true)[0], "TCP", "*", true)
	if err != nil || !strings.HasSuffix(call, "new localport=any enable=yes") {
		t.Fatalf("COM wildcard was not translated for netsh: %s, %v", call, err)
	}
}

func TestFirewallRejectsConflictsBeforeMutation(t *testing.T) {
	base := productRules("3389", true)
	for _, rules := range [][]firewallRule{
		{base[0], {Name: "RdpMux UDP", Description: "Administrator rule", Protocol: 17, Direction: 1}},
		{base[0], base[0]},
		{{Name: "RdpMux TCP", Description: firewallOwner, Protocol: 17, Direction: 1}},
		{{Name: "RdpMux TCP", Description: firewallOwner, Protocol: 6, Direction: 2}},
	} {
		called := false
		err := (&rdpChanges{}).configureFirewall(3390, rules, func(...string) error { called = true; return nil })
		if err == nil || called {
			t.Fatalf("conflicting rules were modified: %+v, %v", rules, err)
		}
	}
}

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
