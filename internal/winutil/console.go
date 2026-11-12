package winutil

import (
	"os"

	"golang.org/x/sys/windows"
)

func ConsoleWrite(text string) {
	if text == "" {
		return
	}
	h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil || h == 0 || h == windows.InvalidHandle {
		return
	}
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		u, err := windows.UTF16FromString(text)
		if err != nil || len(u) < 2 {
			return
		}
		u = u[:len(u)-1]
		for len(u) > 0 {
			var written uint32
			if err := windows.WriteConsole(h, &u[0], uint32(len(u)), &written, nil); err != nil || written == 0 {
				return
			}
			u = u[written:]
		}
	} else {
		os.Stdout.WriteString(text)
	}
}

func ConsoleWln(text string) {
	ConsoleWrite(text + "\r\n")
}
