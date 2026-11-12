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

type OpError struct {
	Op  string
	Err error
}

func (e *OpError) Error() string { return e.Op + ": " + e.Err.Error() }
func (e *OpError) Unwrap() error { return e.Err }

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

func queryServiceState(h windows.Handle) (uint32, error) {
	var status windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &status); err != nil {
		return 0, &OpError{"QueryServiceStatus", err}
	}
	return status.CurrentState, nil
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
	query := func() (uint32, error) { return queryServiceState(h) }
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

// SvcStop also waits for a stop that was already pending.
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
	query := func() (uint32, error) { return queryServiceState(h) }
	state, err := query()
	if err != nil {
		return err
	}
	if state == windows.SERVICE_STOPPED {
		return nil
	}
	if state != windows.SERVICE_STOP_PENDING {
		var status windows.SERVICE_STATUS
		if err := windows.ControlService(h, windows.SERVICE_CONTROL_STOP, &status); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return &OpError{"ControlService", err}
		}
	}
	return waitServiceState(name, windows.SERVICE_STOPPED, query, 60*time.Second)
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
	var ssp windows.SERVICE_STATUS_PROCESS
	var needed uint32
	if err := windows.QueryServiceStatusEx(h, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&ssp)), uint32(unsafe.Sizeof(ssp)), &needed); err != nil {
		return 0, 0, &OpError{"QueryServiceStatusEx", err}
	}
	return ssp.CurrentState, ssp.ProcessId, nil
}
