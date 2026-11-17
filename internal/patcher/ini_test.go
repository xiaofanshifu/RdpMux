package patcher

import (
	"bytes"
	"strings"
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

func TestLoadDB7811(t *testing.T) {
	db, err := LoadDB(assets.ConfigINI, "x64")
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := db.Spec("10.0.19041.7811")
	if !ok {
		t.Fatalf("missing support for 10.0.19041.7811: %v", db.Invalid["10.0.19041.7811"])
	}
	if spec.LocalOnly == nil || spec.SingleUser == nil || spec.DefPolicy == nil || len(spec.SLInitGlobals) != len(slInitOrder) {
		t.Fatalf("incomplete multi-session patches: %+v", spec)
	}
}

// TestEmbeddedLatestVersions locks in the refreshed database: the versions
// added from upstream and the upstream-typo entries corrected in the embedded
// copy must all be supported and complete.
func TestEmbeddedLatestVersions(t *testing.T) {
	db, err := LoadDB(assets.ConfigINI, "x64")
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{
		"10.0.17763.9376", "10.0.26100.33648", "10.0.26100.9818", "10.0.28000.3365",
		"10.0.10240.16384", "10.0.22000.2899", "10.0.27902.1000",
	} {
		spec, ok := db.Spec(version)
		if !ok {
			t.Errorf("%s not supported: %v", version, db.Invalid[version])
			continue
		}
		if spec.LocalOnly == nil || spec.SingleUser == nil || spec.DefPolicy == nil || len(spec.SLInitGlobals) != len(slInitOrder) {
			t.Errorf("%s incomplete: %+v", version, spec)
		}
	}
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

// TestEmbeddedIncompleteEntryRejected guards the real upstream entry that
// carries SingleUser/DefPolicy/SLInit data but no LocalOnly switch: it must be
// rejected as incomplete, not advertised as supported.
func TestEmbeddedIncompleteEntryRejected(t *testing.T) {
	db, err := LoadDB(assets.ConfigINI, "x64")
	if err != nil {
		t.Fatal(err)
	}
	const incomplete = "10.0.26100.8283"
	if _, ok := db.Spec(incomplete); ok {
		t.Fatalf("incomplete entry %s advertised as supported", incomplete)
	}
	if db.Invalid[incomplete] == nil {
		t.Fatalf("incomplete entry %s not marked invalid", incomplete)
	}
	if _, ok := db.Spec("10.0.19041.7811"); !ok {
		t.Fatal("complete entry 10.0.19041.7811 was lost")
	}
}

const switchFixture = `[Main]
Updated=2026-11-16

[SLInit]
bServerSku=1
bRemoteConnAllowed=1
bFUSEnabled=1
bAppServerAllowed=1
bMultimonAllowed=1
lMaxUserSessions=0
ulMaxDebugSessions=0
bInitialized=1

[PatchCodes]
jmpshort=EB
mov_eax_1_nop_2=B8010000009090

[10.0.19041.7811]
LocalOnlyPatch.x64=1
LocalOnlyOffset.x64=93051
LocalOnlyCode.x64=jmpshort
SingleUserPatch.x64=1
SingleUserOffset.x64=1846B
SingleUserCode.x64=mov_eax_1_nop_2
DefPolicyPatch.x64=1
DefPolicyOffset.x64=1F455
DefPolicyCode.x64=jmpshort
SLInitHook.x64=1

[10.0.19041.7811-SLInit]
bServerSku.x64=1260A8
bRemoteConnAllowed.x64=1260BC
bFUSEnabled.x64=1260C8
bAppServerAllowed.x64=1260B4
bMultimonAllowed.x64=1260C0
lMaxUserSessions.x64=1260AC
ulMaxDebugSessions.x64=1260C4
bInitialized.x64=1260A4

[10.0.19041.7725]
LocalOnlyPatch.x64=1
LocalOnlyOffset.x64=93051
LocalOnlyCode.x64=jmpshort
SingleUserPatch.x64=1
SingleUserOffset.x64=1846B
SingleUserCode.x64=mov_eax_1_nop_2
DefPolicyPatch.x64=1
DefPolicyOffset.x64=1F455
DefPolicyCode.x64=jmpshort
SLInitHook.x64=1

[10.0.19041.7725-SLInit]
bServerSku.x64=1260A8
bRemoteConnAllowed.x64=1260BC
bFUSEnabled.x64=1260C8
bAppServerAllowed.x64=1260B4
bMultimonAllowed.x64=1260C0
lMaxUserSessions.x64=1260AC
ulMaxDebugSessions.x64=1260C4
bInitialized.x64=1260A4
`

func TestSwitchCompleteness(t *testing.T) {
	const v = "10.0.19041.7811"
	for _, tt := range []struct {
		name        string
		input       string
		wantSupport bool
		wantLocal   bool
		wantInvalid bool
	}{
		{"complete", switchFixture, true, true, false},
		{"explicit zero disables patch", strings.Replace(switchFixture, "LocalOnlyPatch.x64=1", "LocalOnlyPatch.x64=0", 1), true, false, false},
		{"missing switch", strings.Replace(switchFixture, "LocalOnlyPatch.x64=1\n", "", 1), false, false, true},
		{"misspelled switch", strings.Replace(switchFixture, "LocalOnlyPatch.x64=1", "LocalOnlyPatchx64=1", 1), false, false, true},
		{"invalid switch value", strings.Replace(switchFixture, "LocalOnlyPatch.x64=1", "LocalOnlyPatch.x64=2", 1), false, false, true},
		{"missing SLInitHook", strings.Replace(switchFixture, "SLInitHook.x64=1\n", "", 1), false, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, err := LoadDB([]byte(tt.input), "x64")
			if err != nil {
				t.Fatal(err)
			}
			spec, ok := db.Spec(v)
			if ok != tt.wantSupport {
				t.Fatalf("supported=%v want %v (invalid=%v)", ok, tt.wantSupport, db.Invalid[v])
			}
			if (db.Invalid[v] != nil) != tt.wantInvalid {
				t.Fatalf("invalid=%v want %v", db.Invalid[v] != nil, tt.wantInvalid)
			}
			if ok && (spec.LocalOnly != nil) != tt.wantLocal {
				t.Fatalf("LocalOnly present=%v want %v", spec.LocalOnly != nil, tt.wantLocal)
			}
		})
	}
}

func TestX86OnlyEntrySkippedNotRejected(t *testing.T) {
	input := switchFixture + "\n[10.0.10240.18485]\nLocalOnlyPatch.x86=1\nLocalOnlyOffset.x86=A8048\nLocalOnlyCode.x86=jmpshort\n"
	db, err := LoadDB([]byte(input), "x64")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Spec("10.0.10240.18485"); ok {
		t.Fatal("x86-only entry advertised as x64 supported")
	}
	if db.Invalid["10.0.10240.18485"] != nil {
		t.Fatalf("x86-only entry rejected instead of skipped: %v", db.Invalid["10.0.10240.18485"])
	}
	if _, ok := db.Spec("10.0.19041.7811"); !ok {
		t.Fatal("x64 entry was lost")
	}
}
