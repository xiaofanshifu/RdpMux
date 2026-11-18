package main

//go:generate go run github.com/akavel/rsrc -manifest rdpmuxgui.manifest -o rsrc_windows_amd64.syso

import (
	"os"

	"rdpmux/internal/app"
	"rdpmux/internal/gui"
)

func main() {
	if handled, err := app.ServeOperationHost(os.Args[1:]); handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	gui.Run()
}
