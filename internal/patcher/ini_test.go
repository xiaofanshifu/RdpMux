package patcher

import (
	"bytes"
	"testing"

	"rdpmux/internal/assets"
)

func TestRejectInvalidDatabase(t *testing.T) {
	for _, input := range [][]byte{nil, []byte("<html>error</html>"), []byte("[Main]\nUpdated=2026-11-10\n"), bytes.Replace(assets.ConfigINI, []byte("bRemoteConnAllowed=1"), []byte("missing=1"), 1)} {
		if _, err := LoadDB(input, "x64"); err == nil {
			t.Fatal("accepted invalid database")
		}
	}
}

func TestRejectIncompleteVersionWithoutDisablingOtherVersions(t *testing.T) {
	input := bytes.Replace(assets.ConfigINI, []byte("bServerSku.x64=1260A8"), []byte("invalidName.x64=1260A8"), 1)
	db, err := LoadDB(input, "x64")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Spec("10.0.19041.7725"); ok || db.Invalid["10.0.19041.7725"] == nil {
		t.Fatal("invalid version advertised as supported")
	}
	if _, ok := db.Spec("10.0.22000.2899"); !ok {
		t.Fatal("valid version was lost")
	}
}

func TestEmbeddedGlobalsAreComplete(t *testing.T) {
	db, err := LoadDB(assets.ConfigINI, "x64")
	if err != nil {
		t.Fatal(err)
	}
	for version, spec := range db.Specs {
		if spec.SLInitGlobals == nil {
			continue
		}
		for _, name := range slInitOrder {
			if _, ok := spec.SLInitGlobals[name]; !ok {
				t.Errorf("%s missing %s", version, name)
			}
		}
	}
	for _, version := range []string{"10.0.22000.2899", "10.0.27902.1000"} {
		if _, ok := db.Spec(version); !ok {
			t.Errorf("corrected version %s is unavailable: %v", version, db.Invalid[version])
		}
	}
	if db.Invalid["10.0.10240.19119"] == nil {
		t.Fatal("overlapping global addresses were accepted")
	}
	t.Logf("valid versions=%d, rejected versions=%v", len(db.Specs), db.Invalid)
}

func TestLoadDB7725(t *testing.T) {
	db, err := LoadDB(assets.ConfigINI, "x64")
	if err != nil {
		t.Fatal(err)
	}
	if db.Updated == "" {
		t.Fatal("missing Updated")
	}
	spec, ok := db.Spec("10.0.19041.7725")
	if !ok {
		t.Fatal("no spec for 10.0.19041.7725")
	}
	if spec.LocalOnly == nil || spec.LocalOnly.Offset != 0x93051 || len(spec.LocalOnly.Code) != 1 || spec.LocalOnly.Code[0] != 0xEB {
		t.Fatalf("LocalOnly: %+v", spec.LocalOnly)
	}
	if spec.SingleUser == nil || spec.SingleUser.Offset != 0x1846B || len(spec.SingleUser.Code) != 7 {
		t.Fatalf("SingleUser: %+v", spec.SingleUser)
	}
	if spec.DefPolicy == nil || spec.DefPolicy.Offset != 0x1F455 || len(spec.DefPolicy.Code) != 12 {
		t.Fatalf("DefPolicy: %+v", spec.DefPolicy)
	}
	if spec.SLInitGlobals["bInitialized"] != 0x1260A4 {
		t.Fatalf("SLInit globals: %v", spec.SLInitGlobals)
	}
	if len(spec.SLInitGlobals) != 8 {
		t.Fatalf("want 8 globals, got %d", len(spec.SLInitGlobals))
	}
	if db.SLInitValues["bRemoteConnAllowed"] != 1 || db.SLInitValues["lMaxUserSessions"] != 0 {
		t.Fatalf("SLInit values: %v", db.SLInitValues)
	}
	t.Logf("ini updated %s, %d version specs", db.Updated, len(db.Specs))
}
