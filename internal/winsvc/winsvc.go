package winsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"rdpmux/internal/loc"
	"rdpmux/internal/patcher"
	"rdpmux/internal/winutil"
)

const Name = "RdpMux"
const DisplayName = "RdpMux RDP Patch Service"

func Install(exePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(Name); err == nil {
		s.Close()
		return fmt.Errorf("%s", loc.LF("Installer", "SvcAlreadyInstalled", "service %s already installed", Name))
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return err
	}
	s, err := m.CreateService(Name, exePath, mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		DisplayName:  DisplayName,
		Description:  "Maintains concurrent RDP session patches for termsrv.dll (RdpMux).",
	}, "service")
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
