package sysconfig

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"

	"rdpmux/internal/winutil"
)

type Settings struct {
	AllowTSConnections   bool
	SingleSessionPerUser bool
	HonorLegacySettings  bool
	Port                 uint32
	NLA                  int
	Shadow               int
	HideUsers            bool
}

func readBool(k registry.Key, name string) (bool, bool) {
	v, _, err := k.GetIntegerValue(name)
	if err != nil {
		return false, false
	}
	return v != 0, true
}

func readInt(k registry.Key, name string) (uint32, bool) {
	v, kind, err := k.GetIntegerValue(name)
	if err != nil || kind != registry.DWORD {
		return 0, false
	}
	return uint32(v), true
}

func nlaSelection(security, auth uint32) int {
	switch {
	case security == 0 && auth == 0:
		return 0
	case security == 1 && auth == 0:
		return 1
	case security == 2 && auth == 1:
		return 2
	default:
		return -1 // Preserve combinations the GUI cannot represent.
	}
}

func readRDPPort() (uint32, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, RegRDPTcp, winutil.RegAccess(false))
	if errors.Is(err, registry.ErrNotExist) {
		return 3389, nil
	}
	if err != nil {
		return 0, err
	}
	defer k.Close()
	port, kind, err := k.GetIntegerValue("PortNumber")
	if errors.Is(err, registry.ErrNotExist) {
		return 3389, nil
	}
	if err != nil {
		return 0, err
	}
	if kind != registry.DWORD || port < 1 || port > 65535 {
		return 0, fmt.Errorf("unsupported RDP PortNumber: type=%d value=%d", kind, port)
	}
	return uint32(port), nil
}

func ReadSettings() Settings {
	s := Settings{Port: 3389, NLA: -1, Shadow: -1}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, RegTS, winutil.RegAccess(false)); err == nil {
		if v, ok := readBool(k, "fDenyTSConnections"); ok {
			s.AllowTSConnections = !v
		}
		if v, ok := readBool(k, "fSingleSessionPerUser"); ok {
			s.SingleSessionPerUser = v
		}
		if v, ok := readBool(k, "HonorLegacySettings"); ok {
			s.HonorLegacySettings = v
		}
		k.Close()
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, RegRDPTcp, winutil.RegAccess(false)); err == nil {
		if v, ok := readInt(k, "PortNumber"); ok {
			s.Port = v
		}
		security, ok1 := readInt(k, "SecurityLayer")
		auth, ok2 := readInt(k, "UserAuthentication")
		if ok1 && ok2 {
			s.NLA = nlaSelection(security, auth)
		}
		if v, ok := readInt(k, "Shadow"); ok && v <= 4 {
			s.Shadow = int(v)
		}
		k.Close()
	}
	// Preserve conflicting listener/policy settings when editing unrelated fields.
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, RegTSPolicy, winutil.RegAccess(false)); err == nil {
		v, kind, err := k.GetIntegerValue("Shadow")
		if err == nil {
			if kind != registry.DWORD || v > 4 || int(v) != s.Shadow {
				s.Shadow = -1
			}
		} else if !errors.Is(err, registry.ErrNotExist) {
			s.Shadow = -1
		}
		k.Close()
	} else if !errors.Is(err, registry.ErrNotExist) {
		s.Shadow = -1
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, RegSysPolicy, winutil.RegAccess(false)); err == nil {
		if v, ok := readBool(k, "dontdisplaylastusername"); ok {
			s.HideUsers = v
		}
		k.Close()
	}
	return s
}

func validateSettings(s Settings) error {
	if s.NLA < -1 || s.NLA > 2 {
		return fmt.Errorf("NLA must be between -1 and 2, got %d", s.NLA)
	}
	if s.Shadow < -1 || s.Shadow > 4 {
		return fmt.Errorf("Shadow must be between -1 and 4, got %d", s.Shadow)
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("RDP port must be between 1 and 65535, got %d", s.Port)
	}
	return nil
}

type rdpSetting struct {
	path, name string
	value      uint32
}

// The -1 selections mean "leave unchanged", not a default policy.
func settingsValues(s Settings) []rdpSetting {
	values := []rdpSetting{
		{RegTS, "fDenyTSConnections", boolDWord(!s.AllowTSConnections)},
		{RegTS, "fSingleSessionPerUser", boolDWord(s.SingleSessionPerUser)},
		{RegTS, "HonorLegacySettings", boolDWord(s.HonorLegacySettings)},
		{RegSysPolicy, "dontdisplaylastusername", boolDWord(s.HideUsers)},
	}
	if s.NLA >= 0 {
		auth := uint32(0)
		if s.NLA == 2 {
			auth = 1
		}
		values = append(values, rdpSetting{RegRDPTcp, "SecurityLayer", uint32(s.NLA)}, rdpSetting{RegRDPTcp, "UserAuthentication", auth})
	}
	if s.Shadow >= 0 {
		values = append(values, rdpSetting{RegRDPTcp, "Shadow", uint32(s.Shadow)}, rdpSetting{RegTSPolicy, "Shadow", uint32(s.Shadow)})
	}
	// Commit the port only after the firewall and all other writes succeeded.
	return append(values, rdpSetting{RegRDPTcp, "PortNumber", s.Port})
}

func WriteSettings(s Settings) error {
	if err := validateSettings(s); err != nil {
		return err // Validate external selections before any system access.
	}
	// Read the actual port: the caller's oldPort may be a stale GUI snapshot.
	oldPort, err := readRDPPort()
	if err != nil {
		return err
	}
	changes := &rdpChanges{}
	if oldPort != s.Port {
		rules, err := readRDPFirewallRules()
		if err != nil {
			return err
		}
		if err := changes.configureFirewall(s.Port, rules, runNetsh); err != nil {
			return changes.fail(err)
		}
	}
	for _, value := range settingsValues(s) {
		if err := changes.setValue(value.path, value.name, value.value); err != nil {
			return changes.fail(err)
		}
	}
	return nil
}
