package assets

import _ "embed"

//go:embed files/rfxvmt64.dll
var Rfxvmt64 []byte

//go:embed files/rfxvmt32.dll
var Rfxvmt32 []byte

//go:embed files/rdpwrap.ini
var ConfigINI []byte

//go:embed files/lang/en-US.ini
var LangEnUS []byte

//go:embed files/lang/zh-CN.ini
var LangZhCN []byte

func Lang(code string) []byte {
	switch code {
	case "zh-CN":
		return LangZhCN
	default:
		return LangEnUS
	}
}
