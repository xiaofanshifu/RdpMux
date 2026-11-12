package winutil

import (
	"debug/pe"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var programFilesReplacer = regexp.MustCompile(`(?i)%ProgramFiles%`)

func Is64BitOS() bool {
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		return true
	}
	var processMachine, nativeMachine uint16
	if err := windows.IsWow64Process2(windows.CurrentProcess(), &processMachine, &nativeMachine); err == nil {
		return nativeMachine != pe.IMAGE_FILE_MACHINE_I386
	}
	var wow64 bool
	if err := windows.IsWow64Process(windows.CurrentProcess(), &wow64); err == nil {
		return wow64
	}
	return false
}

func ExpandPath(path string) string {
	if Is64BitOS() {
		path = programFilesReplacer.ReplaceAllString(path, "%ProgramW6432%")
	}
	src, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return path
	}
	buf := make([]uint16, 512)
	n, err := windows.ExpandEnvironmentStrings(src, &buf[0], uint32(len(buf)))
	if err != nil {
		return path
	}
	if int(n) > len(buf) {
		buf = make([]uint16, n)
		n, err = windows.ExpandEnvironmentStrings(src, &buf[0], uint32(len(buf)))
		if err != nil {
			return path
		}
	}
	return windows.UTF16ToString(buf[:n])
}

func ExpandSystemPath(elem ...string) string {
	root := ExpandPath("%SystemRoot%")
	parts := append([]string{root}, elem...)
	return strings.Join(parts, `\`)
}

func RegAccess(write bool) uint32 {
	access := uint32(registry.READ)
	if write {
		access = registry.WRITE
	}
	if Is64BitOS() {
		access |= registry.WOW64_64KEY
	}
	return access
}
