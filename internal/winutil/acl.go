package winutil

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procSetEntriesInAclW = windows.NewLazySystemDLL("advapi32.dll").NewProc("SetEntriesInAclW")

func GrantSidFullAccess(path, sidStr string) error {
	var sid *windows.SID
	s, err := windows.UTF16PtrFromString(sidStr)
	if err != nil {
		return err
	}
	if err := windows.ConvertStringSidToSid(s, &sid); err != nil {
		return &OpError{"ConvertStringSidToSid", err}
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(sid)))
	ea := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	var dacl *windows.ACL
	ret, _, _ := procSetEntriesInAclW.Call(1, uintptr(unsafe.Pointer(&ea)), 0, uintptr(unsafe.Pointer(&dacl)))
	runtime.KeepAlive(ea)
	runtime.KeepAlive(sid)
	if ret != 0 {
		return &OpError{"SetEntriesInAcl", windows.Errno(ret)}
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(dacl)))
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return &OpError{"SetNamedSecurityInfo", err}
	}
	return nil
}
