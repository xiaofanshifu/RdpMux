package main

//go:generate go run github.com/akavel/rsrc -manifest rdpmuxgui.manifest -o rsrc_windows_amd64.syso

import "rdpmux/internal/gui"

func main() { gui.Run() }
