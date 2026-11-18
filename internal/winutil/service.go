package winutil

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const SC_ENUM_PROCESS_INFO = 0

// stopBeginTimeout bounds how long a stop request may take to take effect. The
// SCM marks a service STOP_PENDING as soon as it processes the control, so a
// service that is still RUNNING after this long never began stopping.
const stopBeginTimeout = 10 * time.Second

type OpError struct {
	Op  string
	Err error
}

func (e *OpError) Error() string { return e.Op + ": " + e.Err.Error() }
func (e *OpError) Unwrap() error { return e.Err }

// ErrServiceReplaced reports that the service instance changed while a stop was
// pending: the service was started again (for example by SCM failure recovery)
// before it reported Stopped. A new instance runs unpatched code, so callers
// that stop a service to clear in-memory patches can treat it as a restart.
var ErrServiceReplaced = errors.New("service instance was replaced")

// ErrStopNotStarted reports that a stop request never took effect: the service
// kept running without reporting a stop transition. Waiting for the full
// timeout cannot succeed, so the wait fails as soon as the grace period ends.
// A service that restarts within its own process (same pid) and completes the
// cycle between polls is indistinguishable from an ignored request and is
// reported instead of being assumed to have restarted.
var ErrStopNotStarted = errors.New("service did not begin stopping")

func openSCM(access uint32) (windows.Handle, error) {
	h, err := windows.OpenSCManager(nil, nil, access)
	if err != nil {
		return 0, &OpError{"OpenSCManager", err}
	}
	return h, nil
}

func openService(scm windows.Handle, name string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	h, err := windows.OpenService(scm, p, access)
	if err != nil {
		return 0, &OpError{"OpenService", err}
	}
	return h, nil
}

func SvcGetStart(name string) (uint32, error) {
	scm, err := openSCM(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, err
	}
	defer windows.CloseServiceHandle(scm)
	h, err := openService(scm, name, windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return 0, err
	}
	defer windows.CloseServiceHandle(h)
	var needed uint32
	if err := windows.QueryServiceConfig(h, nil, 0, &needed); err != nil && !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return 0, &OpError{"QueryServiceConfig", err}
	}
	if needed == 0 {
		return 0, windows.ERROR_INVALID_DATA
	}
	buf := make([]byte, needed)
	cfg := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buf[0]))
	if err := windows.QueryServiceConfig(h, cfg, needed, &needed); err != nil {
		return 0, &OpError{"QueryServiceConfig", err}
	}
	return cfg.StartType, nil
}

func SvcSetStart(name string, startType uint32) error {
	scm, err := openSCM(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(scm)
	h, err := openService(scm, name, windows.SERVICE_CHANGE_CONFIG)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(h)
	if err := windows.ChangeServiceConfig(h, windows.SERVICE_NO_CHANGE, startType, windows.SERVICE_NO_CHANGE,
		nil, nil, nil, nil, nil, nil, nil); err != nil {
		return &OpError{"ChangeServiceConfig", err}
	}
	return nil
}

// ServiceStatus is a snapshot of the service instance behind a handle.
type ServiceStatus struct {
	State uint32
	PID   uint32
}

func queryServiceStatus(h windows.Handle) (ServiceStatus, error) {
	var ssp windows.SERVICE_STATUS_PROCESS
	var needed uint32
	if err := windows.QueryServiceStatusEx(h, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&ssp)), uint32(unsafe.Sizeof(ssp)), &needed); err != nil {
		return ServiceStatus{}, &OpError{"QueryServiceStatusEx", err}
	}
	return ServiceStatus{State: ssp.CurrentState, PID: ssp.ProcessId}, nil
}

func waitServiceState(name string, target uint32, query func() (uint32, error), timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		state, err := query()
		if err != nil {
			return err
		}
		if state == target {
			return nil
		}
		if target == windows.SERVICE_RUNNING && state == windows.SERVICE_STOPPED {
			return fmt.Errorf("%s stopped before reaching Running", name)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%s did not reach state %d within %s: %w", name, target, timeout, windows.ERROR_SERVICE_REQUEST_TIMEOUT)
		}
		time.Sleep(min(100*time.Millisecond, remaining))
	}
}

// SvcStart returns only after the service reaches Running.
func SvcStart(name string) error {
	scm, err := openSCM(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(scm)
	h, err := openService(scm, name, windows.SERVICE_START|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(h)
	query := func() (uint32, error) {
		st, err := queryServiceStatus(h)
		return st.State, err
	}
	state, err := query()
	if err != nil {
		return err
	}
	if state == windows.SERVICE_RUNNING {
		return nil
	}
	if state == windows.SERVICE_STOP_PENDING {
		if err := waitServiceState(name, windows.SERVICE_STOPPED, query, 60*time.Second); err != nil {
			return err
		}
	}
	if state != windows.SERVICE_START_PENDING {
		if err := windows.StartService(h, 0, nil); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return &OpError{"StartService", err}
		}
	}
	return waitServiceState(name, windows.SERVICE_RUNNING, query, 60*time.Second)
}

// SvcStop also waits for a stop that was already pending. A request that never
// takes effect is reported as ErrStopNotStarted instead of waiting out the
// full timeout.
func SvcStop(name string) error {
	scm, err := openSCM(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(scm)
	h, err := openService(scm, name, windows.SERVICE_STOP|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(h)
	query := func() (ServiceStatus, error) { return queryServiceStatus(h) }
	st, err := query()
	if err != nil {
		return err
	}
	if st.State == windows.SERVICE_STOPPED {
		return nil
	}
	if st.State == windows.SERVICE_START_PENDING {
		// The SCM rejects a stop control while a service is still starting, so
		// wait for it to become controllable instead of failing immediately.
		if err := waitServiceState(name, windows.SERVICE_RUNNING, func() (uint32, error) {
			cur, err := query()
			return cur.State, err
		}, 15*time.Second); err != nil {
			if cur, qerr := query(); qerr == nil && cur.State == windows.SERVICE_STOPPED {
				return nil // The pending start failed; the service is already stopped.
			}
			return err
		}
		if st, err = query(); err != nil {
			return err
		}
		if st.State == windows.SERVICE_STOPPED {
			return nil
		}
	}
	if st.State != windows.SERVICE_STOP_PENDING {
		var status windows.SERVICE_STATUS
		if err := windows.ControlService(h, windows.SERVICE_CONTROL_STOP, &status); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return &OpError{"ControlService", err}
		}
	}
	return waitServiceStopped(name, st.PID, query, 60*time.Second)
}

// waitServiceStopped reports ErrServiceReplaced as soon as the instance behind
// the handle changes, instead of waiting out the timeout for a state the old
// instance will never reach. A request that never begins stopping is reported
// after stopBeginTimeout rather than at the end of the full timeout.
func waitServiceStopped(name string, wantPID uint32, query func() (ServiceStatus, error), timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	begin := time.Now()
	beginDeadline := min(timeout, stopBeginTimeout)
	sawPending := false
	for {
		st, err := query()
		if err != nil {
			return err
		}
		if st.State == windows.SERVICE_STOPPED {
			return nil
		}
		if st.PID != 0 && wantPID != 0 && st.PID != wantPID {
			return fmt.Errorf("%s restarted instead of stopping (pid %d -> %d): %w", name, wantPID, st.PID, ErrServiceReplaced)
		}
		if st.State == windows.SERVICE_STOP_PENDING {
			sawPending = true
		}
		if sawPending && (st.State == windows.SERVICE_RUNNING || st.State == windows.SERVICE_START_PENDING) {
			return fmt.Errorf("%s restarted instead of stopping (state %d): %w", name, st.State, ErrServiceReplaced)
		}
		if !sawPending && time.Since(begin) > beginDeadline {
			return fmt.Errorf("%s did not begin stopping within %s: %w", name, beginDeadline, ErrStopNotStarted)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%s did not reach state %d within %s: %w", name, windows.SERVICE_STOPPED, timeout, windows.ERROR_SERVICE_REQUEST_TIMEOUT)
		}
		time.Sleep(min(100*time.Millisecond, remaining))
	}
}

type ServiceInfo struct {
	Name string
	PID  uint32
}

func EnumServices() ([]ServiceInfo, error) {
	scm, err := openSCM(windows.SC_MANAGER_CONNECT | windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, err
	}
	defer windows.CloseServiceHandle(scm)
	// The API caps each page at 256 KiB; needed describes only the remaining data.
	buf := make([]byte, 256*1024)
	return enumServicePages(func(resume *uint32) ([]ServiceInfo, error) {
		var needed, returned uint32
		err := windows.EnumServicesStatusEx(scm, SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32, windows.SERVICE_STATE_ALL,
			&buf[0], uint32(len(buf)), &needed, &returned, resume, nil)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return nil, &OpError{"EnumServicesStatusEx", err}
		}
		type enumServiceStatusProcess struct {
			serviceName   *uint16
			displayName   *uint16
			serviceType   uint32
			currentState  uint32
			controls      uint32
			win32ExitCode uint32
			specificExit  uint32
			checkPoint    uint32
			waitHint      uint32
			processID     uint32
			serviceFlags  uint32
		}
		entries := unsafe.Slice((*enumServiceStatusProcess)(unsafe.Pointer(&buf[0])), int(returned))
		services := make([]ServiceInfo, 0, len(entries))
		for _, e := range entries {
			services = append(services, ServiceInfo{
				Name: windows.UTF16PtrToString(e.serviceName),
				PID:  e.processID,
			})
		}
		return services, err
	})
}

func enumServicePages(next func(*uint32) ([]ServiceInfo, error)) ([]ServiceInfo, error) {
	var services []ServiceInfo
	var resume uint32
	for {
		previous := resume
		page, err := next(&resume)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return nil, err
		}
		services = append(services, page...)
		if err == nil {
			return services, nil
		}
		if resume == previous {
			return nil, &OpError{"EnumServicesStatusEx made no progress", windows.ERROR_MORE_DATA}
		}
	}
}

func FindTermService() (pid uint32, shared []string, err error) {
	services, err := EnumServices()
	if err != nil {
		return 0, nil, err
	}
	found := false
	for _, s := range services {
		if strings.EqualFold(s.Name, "TermService") {
			pid = s.PID
			found = true
			break
		}
	}
	if !found {
		return 0, nil, windows.ERROR_SERVICE_DOES_NOT_EXIST
	}
	if pid == 0 {
		return 0, nil, nil // Stopped services do not share a process.
	}
	for _, s := range services {
		if s.PID == pid && !strings.EqualFold(s.Name, "TermService") {
			shared = append(shared, s.Name)
		}
	}
	return pid, shared, nil
}

// DependentServices returns the currently active services that depend on name.
func DependentServices(name string) ([]string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return nil, &OpError{"OpenService", err}
	}
	defer s.Close()
	return s.ListDependentServices(svc.Active)
}

func SvcQueryState(name string) (state uint32, pid uint32, err error) {
	scm, err := openSCM(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseServiceHandle(scm)
	h, err := openService(scm, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseServiceHandle(h)
	st, err := queryServiceStatus(h)
	if err != nil {
		return 0, 0, err
	}
	return st.State, st.PID, nil
}
