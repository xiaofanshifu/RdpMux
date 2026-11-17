package sysconfig

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"rdpmux/internal/assets"
	"rdpmux/internal/winutil"
)

const (
	RegTS        = `SYSTEM\CurrentControlSet\Control\Terminal Server`
	RegRDPTcp    = RegTS + `\WinStations\RDP-Tcp`
	RegTSPolicy  = `SOFTWARE\Policies\Microsoft\Windows NT\Terminal Services`
	RegSysPolicy = `SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`
)

type rdpChanges struct {
	undo []func() error
}

func (c *rdpChanges) rollback() error {
	var errs []error
	for i := len(c.undo) - 1; i >= 0; i-- {
		if err := c.undo[i](); err != nil {
			errs = append(errs, fmt.Errorf("restore RDP configuration: %w", err))
		}
	}
	c.undo = nil
	return errors.Join(errs...)
}

func (c *rdpChanges) fail(err error) error {
	return errors.Join(err, c.rollback())
}

func (c *rdpChanges) openKey(path string) (registry.Key, error) {
	access := winutil.RegAccess(true) | registry.READ
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, access)
	if !errors.Is(err, registry.ErrNotExist) {
		return k, err
	}
	index := strings.LastIndex(path, `\`)
	if index < 0 {
		return 0, err
	}
	parentPath, name := path[:index], path[index+1:]
	parent, err := c.openKey(parentPath)
	if err != nil {
		return 0, err
	}
	defer parent.Close()
	k, existed, err := registry.CreateKey(parent, name, access)
	if err != nil {
		return 0, err
	}
	if !existed {
		c.undo = append(c.undo, func() error {
			parent, err := registry.OpenKey(registry.LOCAL_MACHINE, parentPath, access)
			if errors.Is(err, registry.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			defer parent.Close()
			child, err := registry.OpenKey(parent, name, access)
			if errors.Is(err, registry.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			info, err := child.Stat()
			child.Close()
			if err != nil {
				return err
			}
			// Never recursively delete data written by somebody else.
			if info.SubKeyCount != 0 || info.ValueCount != 0 {
				return fmt.Errorf("new registry key %s is no longer empty", path)
			}
			return registry.DeleteKey(parent, name)
		})
	}
	return k, nil
}

func putRDPValue(k registry.Key, name string, value any) error {
	switch v := value.(type) {
	case nil:
		err := k.DeleteValue(name)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	case uint32:
		return k.SetDWordValue(name, v)
	case string:
		return k.SetStringValue(name, v)
	default:
		return fmt.Errorf("unsupported RDP registry value %T", value)
	}
}

func (c *rdpChanges) setValue(path, name string, value any) error {
	k, err := c.openKey(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer k.Close()
	var old any
	switch value.(type) {
	case uint32:
		var v uint64
		var kind uint32
		v, kind, err = k.GetIntegerValue(name)
		if err == nil && kind != registry.DWORD {
			err = registry.ErrUnexpectedType
		}
		old = uint32(v)
	case string:
		var v string
		var kind uint32
		v, kind, err = k.GetStringValue(name)
		if err == nil && kind != registry.SZ {
			err = registry.ErrUnexpectedType
		}
		old = v
	default:
		return fmt.Errorf("unsupported RDP registry value %T", value)
	}
	if errors.Is(err, registry.ErrNotExist) {
		old = nil
	} else if err != nil {
		return fmt.Errorf("read %s\\%s: %w", path, name, err)
	}
	if old == value {
		return nil
	}
	c.undo = append(c.undo, func() error {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, winutil.RegAccess(true))
		if errors.Is(err, registry.ErrNotExist) && old == nil {
			return nil
		}
		if err != nil {
			return err
		}
		defer k.Close()
		if err := putRDPValue(k, name, old); err != nil {
			return fmt.Errorf("%s\\%s: %w", path, name, err)
		}
		return nil
	})
	if err := putRDPValue(k, name, value); err != nil {
		return fmt.Errorf("write %s\\%s: %w", path, name, err)
	}
	return nil
}

func boolDWord(value bool) uint32 {
	if value {
		return 1
	}
	return 0
}

func EnableRDP() (undo func() error, err error) {
	changes := &rdpChanges{}
	defer func() {
		if err != nil {
			err = changes.fail(err)
		}
	}()
	for _, value := range []rdpSetting{
		{RegTS, "fDenyTSConnections", 0},
		{RegTS + `\Licensing Core`, "EnableConcurrentSessions", 1},
		{`SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon`, "AllowMultipleTSSessions", 1},
	} {
		if err := changes.setValue(value.path, value.name, value.value); err != nil {
			return nil, err
		}
	}
	if err := changes.ensureAddIns(); err != nil {
		return nil, err
	}
	for _, name := range []string{"CertPropSvc", "SessionEnv"} {
		start, err := winutil.SvcGetStart(name)
		if err != nil {
			return nil, fmt.Errorf("query %s startup type: %w", name, err)
		}
		if start == windows.SERVICE_DISABLED {
			changes.undo = append(changes.undo, func() error { return winutil.SvcSetStart(name, start) })
			if err := winutil.SvcSetStart(name, windows.SERVICE_DEMAND_START); err != nil {
				return nil, fmt.Errorf("configure %s startup type: %w", name, err)
			}
		}
	}
	return changes.rollback, nil
}

func (c *rdpChanges) ensureAddIns() error {
	addIns := RegTS + `\AddIns`
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, addIns, winutil.RegAccess(false)); err == nil {
		k.Close()
		return nil
	} else if !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	for _, a := range []struct {
		key, name string
		typeVal   uint32
	}{
		{`Clip Redirector`, "RDPClip", 3},
		{`DND Redirector`, "RDPDND", 3},
		{`Dynamic VC`, "", 0xFFFFFFFF},
	} {
		path := addIns + `\` + a.key
		if a.name != "" {
			if err := c.setValue(path, "Name", a.name); err != nil {
				return err
			}
		}
		if err := c.setValue(path, "Type", a.typeVal); err != nil {
			return err
		}
	}
	return nil
}

func DropRfxvmt() error {
	target := winutil.ExpandSystemPath("System32", "rfxvmt.dll")
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	data := assets.Rfxvmt32
	if winutil.Is64BitOS() {
		data = assets.Rfxvmt64
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	err = errors.Join(writeErr, f.Close())
	if err != nil {
		return errors.Join(err, os.Remove(target))
	}
	return nil
}

func TermServiceState() (state uint32, pid uint32, err error) {
	return winutil.SvcQueryState("TermService")
}

type termServiceOps struct {
	find       func() (uint32, []string, error)
	dependents func(string) ([]string, error)
	query      func(string) (uint32, uint32, error)
	stop       func(string) error
	start      func(string) error
}

func RestartTermService() error {
	return restartTermService(termServiceOps{
		find: winutil.FindTermService, dependents: winutil.DependentServices,
		query: winutil.SvcQueryState, stop: winutil.SvcStop, start: winutil.SvcStart,
	})
}

func restartTermService(ops termServiceOps) error {
	_, shared, err := ops.find()
	if err != nil {
		return fmt.Errorf("enumerate TermService process: %w", err)
	}
	var names []string
	seen := map[string]bool{}
	var visit func(string) error
	visit = func(name string) error {
		key := strings.ToLower(name)
		if seen[key] {
			return nil
		}
		seen[key] = true
		state, _, err := ops.query(name)
		if err != nil {
			return fmt.Errorf("query %s: %w", name, err)
		}
		if state != windows.SERVICE_STOPPED {
			deps, err := ops.dependents(name)
			if err != nil {
				return fmt.Errorf("enumerate dependents of %s: %w", name, err)
			}
			for _, dep := range deps {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		if state != windows.SERVICE_STOPPED || key == "termservice" {
			names = append(names, name)
		}
		return nil
	}
	// Resolve dependencies before stopping any service.
	for _, name := range append(shared, "TermService") {
		if err := visit(name); err != nil {
			return err
		}
	}
	var restore []string
	var errs []error
	for _, name := range names {
		// A timed-out stop may still complete and need restoration.
		restore = append(restore, name)
		if err := ops.stop(name); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", name, err))
			break // Do not proceed to stop its dependencies after a failure.
		}
	}
	for i := len(restore) - 1; i >= 0; i-- {
		name := restore[i]
		if err := ops.start(name); err != nil {
			errs = append(errs, fmt.Errorf("start %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func InstallDir() string {
	return winutil.ExpandPath(`%ProgramFiles%\RdpMux`)
}

var (
	winsta                  = windows.NewLazySystemDLL("winsta.dll")
	procWinStationEnumerate = winsta.NewProc("WinStationEnumerateW")
	procWinStationFreeMem   = winsta.NewProc("WinStationFreeMemory")
)

type winStationInfo struct {
	SessionID uint32
	Name      [34]uint16
	State     uint32
}

func IsListenerWorking() bool {
	var sessions *winStationInfo
	var count uint32
	ret, _, _ := procWinStationEnumerate.Call(0, uintptr(unsafe.Pointer(&sessions)), uintptr(unsafe.Pointer(&count)))
	if ret == 0 || sessions == nil {
		return false
	}
	defer procWinStationFreeMem.Call(uintptr(unsafe.Pointer(sessions)))
	list := unsafe.Slice(sessions, int(count))
	for i := range list {
		if windows.UTF16ToString(list[i].Name[:]) == "RDP-Tcp" {
			return true
		}
	}
	return false
}

func Arch() string {
	if runtime.GOARCH == "amd64" || winutil.Is64BitOS() {
		return "x64"
	}
	return "x86"
}
