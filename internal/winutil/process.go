package winutil

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var adjustTokenPrivileges = windows.NewLazySystemDLL("advapi32.dll").NewProc("AdjustTokenPrivileges")

func EnablePrivilege(name string) error {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token)
	if err != nil {
		return &OpError{"OpenProcessToken", err}
	}
	defer token.Close()
	var luid windows.LUID
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	if err := windows.LookupPrivilegeValue(nil, namePtr, &luid); err != nil {
		return &OpError{"LookupPrivilegeValue", err}
	}
	tp := windows.Tokenprivileges{
		PrivilegeCount: 1,
		Privileges: [1]windows.LUIDAndAttributes{
			{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED},
		},
	}
	result, _, callErr := adjustTokenPrivileges.Call(uintptr(token), 0, uintptr(unsafe.Pointer(&tp)), 0, 0, 0)
	if result == 0 || errors.Is(callErr, windows.ERROR_NOT_ALL_ASSIGNED) {
		return &OpError{"AdjustTokenPrivileges", callErr}
	}
	return nil
}

// DeleteFileOnReboot schedules a file or directory for deletion after the
// next reboot. It is used for files that are locked by the running image.
func DeleteFileOnReboot(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(p, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT); err != nil {
		return &OpError{"MoveFileEx", err}
	}
	return nil
}
