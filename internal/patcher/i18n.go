package patcher

import "rdpmux/internal/loc"

func tr(key, def string, args ...any) string {
	return loc.LF("Patcher", key, def, args...)
}
