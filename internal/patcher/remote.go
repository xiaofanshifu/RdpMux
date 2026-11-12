package patcher

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var kernel32 = windows.NewLazySystemDLL("kernel32.dll")
var virtualProtectEx = kernel32.NewProc("VirtualProtectEx")
var flushInstructionCache = kernel32.NewProc("FlushInstructionCache")

type Process struct {
	Handle windows.Handle
	PID    uint32
	mutex  windows.Handle
}

func OpenProcess(pid uint32) (*Process, error) {
	h, err := windows.OpenProcess(windows.PROCESS_VM_READ|windows.PROCESS_VM_WRITE|windows.PROCESS_VM_OPERATION|windows.PROCESS_QUERY_INFORMATION, false, pid)
	if err != nil {
		return nil, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`Global\RdpMux.Patch.%d`, pid))
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	mutex, err := windows.CreateMutex(nil, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(h)
		return nil, err
	}
	return &Process{Handle: h, PID: pid, mutex: mutex}, nil
}

func (p *Process) Lock() error {
	runtime.LockOSThread()
	state, err := windows.WaitForSingleObject(p.mutex, 15000)
	if err == nil && (state == windows.WAIT_OBJECT_0 || state == windows.WAIT_ABANDONED) {
		return nil
	}
	runtime.UnlockOSThread()
	if err != nil {
		return err
	}
	return fmt.Errorf("timed out waiting for patch writer")
}

func (p *Process) Unlock() error {
	err := windows.ReleaseMutex(p.mutex)
	runtime.UnlockOSThread()
	return err
}

func (p *Process) Close() {
	windows.CloseHandle(p.mutex)
	windows.CloseHandle(p.Handle)
}

func (p *Process) Read(addr uintptr, buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	var read uintptr
	if err := windows.ReadProcessMemory(p.Handle, addr, &buf[0], uintptr(len(buf)), &read); err != nil {
		return err
	}
	if int(read) != len(buf) {
		return fmt.Errorf("short read at 0x%X: %d/%d", addr, read, len(buf))
	}
	return nil
}

func (p *Process) Write(addr uintptr, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var written uintptr
	if err := windows.WriteProcessMemory(p.Handle, addr, &data[0], uintptr(len(data)), &written); err != nil {
		return err
	}
	if int(written) != len(data) {
		return fmt.Errorf("short write at 0x%X: %d/%d", addr, written, len(data))
	}
	return nil
}

func (p *Process) WriteCode(addr uintptr, data []byte) (err error) {
	if len(data) == 0 {
		return nil
	}
	var oldProtect uint32
	ok, _, callErr := virtualProtectEx.Call(uintptr(p.Handle), addr, uintptr(len(data)), windows.PAGE_EXECUTE_READWRITE, uintptr(unsafe.Pointer(&oldProtect)))
	if ok == 0 {
		return fmt.Errorf("VirtualProtectEx: %w", callErr)
	}
	defer func() {
		var unused uint32
		ok, _, restoreErr := virtualProtectEx.Call(uintptr(p.Handle), addr, uintptr(len(data)), uintptr(oldProtect), uintptr(unsafe.Pointer(&unused)))
		if ok == 0 {
			err = errors.Join(err, fmt.Errorf("restore page protection: %w", restoreErr))
		}
	}()
	err = p.Write(addr, data)
	ok, _, flushErr := flushInstructionCache.Call(uintptr(p.Handle), addr, uintptr(len(data)))
	if ok == 0 {
		err = errors.Join(err, fmt.Errorf("FlushInstructionCache: %w", flushErr))
	}
	return err
}

func (p *Process) ReadU32(addr uintptr) (uint32, error) {
	var buf [4]byte
	err := p.Read(addr, buf[:])
	return uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24, err
}

func FindTermsrv(pid uint32) (base uintptr, path string, err error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPMODULE, pid)
	if err != nil {
		return 0, "", fmt.Errorf("CreateToolhelp32Snapshot(%d): %w", pid, err)
	}
	defer windows.CloseHandle(snap)
	var me windows.ModuleEntry32
	me.Size = uint32(unsafe.Sizeof(me))
	if err := windows.Module32First(snap, &me); err != nil {
		return 0, "", err
	}
	for {
		name := windows.UTF16ToString(me.Module[:])
		if strings.EqualFold(name, "termsrv.dll") {
			return me.ModBaseAddr, windows.UTF16ToString(me.ExePath[:]), nil
		}
		if err := windows.Module32Next(snap, &me); err != nil {
			break
		}
	}
	return 0, "", fmt.Errorf("termsrv.dll not loaded in process %d", pid)
}
