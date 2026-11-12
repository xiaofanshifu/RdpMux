package winutil

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

type FileVersion struct {
	Major, Minor, Release, Build uint16
}

func (v FileVersion) String() string {
	return fmt.Sprintf("%d.%d.%d.%d", v.Major, v.Minor, v.Release, v.Build)
}

func GetFileVersion(path string) (FileVersion, error) {
	var v FileVersion
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil {
		return v, err
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return v, err
	}
	var ptr unsafe.Pointer
	var n uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&ptr), &n); err != nil {
		return v, err
	}
	info := (*windows.VS_FIXEDFILEINFO)(ptr)
	return FileVersion{
		Major:   uint16(info.FileVersionMS >> 16),
		Minor:   uint16(info.FileVersionMS),
		Release: uint16(info.FileVersionLS >> 16),
		Build:   uint16(info.FileVersionLS),
	}, nil
}
