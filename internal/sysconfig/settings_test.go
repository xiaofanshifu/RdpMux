package sysconfig

import "testing"

func TestSettingsValidation(t *testing.T) {
	for nla := -1; nla <= 2; nla++ {
		for shadow := -1; shadow <= 4; shadow++ {
			for _, port := range []uint32{1, 3389, 65535} {
				if err := validateSettings(Settings{NLA: nla, Shadow: shadow, Port: port}); err != nil {
					t.Fatalf("valid setting rejected: %v", err)
				}
			}
		}
	}
	for _, s := range []Settings{
		{NLA: -2, Shadow: -1, Port: 3389},
		{NLA: 3, Shadow: -1, Port: 3389},
		{NLA: -1, Shadow: -2, Port: 3389},
		{NLA: -1, Shadow: 5, Port: 3389},
		{NLA: -1, Shadow: -1, Port: 0},
		{NLA: -1, Shadow: -1, Port: 65536},
		{NLA: -1, Shadow: -1, Port: ^uint32(0)},
	} {
		// Invalid input must return before registry or firewall access.
		if err := WriteSettings(s); err == nil {
			t.Fatalf("invalid settings accepted: %+v", s)
		}
	}
}

func TestUnsupportedSelectionsRemainUntouched(t *testing.T) {
	for _, tc := range []struct {
		security, auth uint32
		want           int
	}{
		{0, 0, 0}, {1, 0, 1}, {2, 1, 2},
		{0, 1, -1}, {1, 1, -1}, {2, 0, -1}, {3, 1, -1}, {2, 2, -1},
	} {
		if got := nlaSelection(tc.security, tc.auth); got != tc.want {
			t.Errorf("NLA(%d, %d) = %d, want %d", tc.security, tc.auth, got, tc.want)
		}
	}
	values := settingsValues(Settings{NLA: -1, Shadow: -1, Port: 3390})
	for _, value := range values {
		if value.name == "SecurityLayer" || value.name == "UserAuthentication" || value.name == "Shadow" {
			t.Fatalf("unsupported selection would be overwritten: %+v", value)
		}
	}
	if last := values[len(values)-1]; last.name != "PortNumber" || last.value != 3390 {
		t.Fatalf("port must be written last: %+v", values)
	}
}

func TestSupportedSettingsValues(t *testing.T) {
	for nla := 0; nla <= 2; nla++ {
		values := settingsValues(Settings{NLA: nla, Shadow: 4, Port: 3389})
		shadowCount := 0
		foundSecurity, foundAuth := false, false
		for _, value := range values {
			switch value.name {
			case "SecurityLayer":
				foundSecurity = value.value == uint32(nla)
			case "UserAuthentication":
				foundAuth = value.value == boolDWord(nla == 2)
			case "Shadow":
				if value.value != 4 {
					t.Fatalf("incorrect Shadow value: %+v", value)
				}
				shadowCount++
			}
		}
		if !foundSecurity || !foundAuth || shadowCount != 2 {
			t.Fatalf("incorrect settings for NLA %d: %+v", nla, values)
		}
	}
}
