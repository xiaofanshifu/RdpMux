package winsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"rdpmux/internal/patcher"
	"rdpmux/internal/winutil"
)

const Name = "RdpMux"
const DisplayName = "RdpMux RDP Patch Service"

// Install registers the service, or updates the registration in place when it
// already exists, so installing again repairs the installation instead of
// failing. The caller stops the service before replacing its binary.
func Install(exePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	cfg := mgr.Config{
		ServiceType:    windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:      mgr.StartAutomatic,
		ErrorControl:   mgr.ErrorNormal,
		DisplayName:    DisplayName,
		Description:    "Maintains concurrent RDP session patches for termsrv.dll (RdpMux).",
		BinaryPathName: syscall.EscapeArg(exePath) + " service",
	}
	if s, err := m.OpenService(Name); err == nil {
		defer s.Close()
		return s.UpdateConfig(cfg)
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return err
	}
	s, err := m.CreateService(Name, exePath, cfg, "service")
	if err != nil {
		return err
	}
	s.Close()
	return nil
}

func Remove() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(Name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer s.Close()
	if err := Stop(); err != nil {
		return err
	}
	return s.Delete()
}

func Start() error {
	return winutil.SvcStart(Name)
}

func Stop() error {
	return winutil.SvcStop(Name)
}

func Restart() error {
	if err := Stop(); err != nil {
		return err
	}
	return Start()
}

// StartOneShot creates a demand-start service that runs exePath with args and
// starts it. The returned cleanup deletes the service record; the started
// process should delete it too, because the caller may not survive the work.
func StartOneShot(name, exePath string, args ...string) (func(), error) {
	// Hold the image without write or delete sharing until the service has
	// started. Otherwise anyone who can write to exePath's directory could
	// replace it and have the replacement run as LocalSystem.
	guard, err := lockImage(exePath)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(guard)
	m, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	if s, err := m.OpenService(name); err == nil {
		st, queryErr := s.Query()
		if queryErr == nil && st.State != svc.Stopped {
			s.Close()
			m.Disconnect()
			return nil, fmt.Errorf("service %s is still running", name)
		}
		_ = s.Delete()
		s.Close()
	}
	s, err := m.CreateService(name, exePath, mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartManual,
		ErrorControl: mgr.ErrorNormal,
		DisplayName:  name,
	}, args...)
	if err != nil {
		m.Disconnect()
		return nil, err
	}
	if err := s.Start(); err != nil {
		_ = s.Delete()
		s.Close()
		m.Disconnect()
		return nil, err
	}
	return func() {
		_ = s.Delete()
		s.Close()
		m.Disconnect()
	}, nil
}

func lockImage(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
}

// DeleteOneShot removes a service created by StartOneShot.
func DeleteOneShot(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Delete()
}

func State() (svc.State, error) {
	st, _, err := winutil.SvcQueryState(Name)
	if err != nil {
		return 0, err
	}
	return svc.State(st), nil
}

type Handler struct {
	Watcher *patcher.Watcher
}

func (h *Handler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Watcher.Run(ctx)
		close(done)
	}()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for req := range r {
		switch req.Cmd {
		case svc.Interrogate:
			changes <- req.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			cancel()
			<-done
			return false, 0
		}
	}
	cancel()
	<-done
	return false, 0
}

func Run(w *patcher.Watcher) error {
	return svc.Run(Name, &Handler{Watcher: w})
}

func IniPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "rdpwrap.ini"
	}
	return filepath.Join(filepath.Dir(exe), "rdpwrap.ini")
}
