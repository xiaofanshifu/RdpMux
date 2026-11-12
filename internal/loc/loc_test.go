package loc

import (
	"sync"
	"testing"

	"rdpmux/internal/assets"
)

func withLanguageState(t *testing.T) {
	t.Helper()
	old := current.Load()
	t.Cleanup(func() { current.Store(old) })
}

func TestNormalizeLanguageCode(t *testing.T) {
	tests := map[string]string{
		"zh-CN":   "zh-CN",
		"zh-Hans": "zh-CN",
		"en-US":   "en-US",
		"en-GB":   "en-US",
		"fr-FR":   "",
		"":        "",
	}
	for input, want := range tests {
		if got := NormalizeLanguageCode(input); got != want {
			t.Errorf("NormalizeLanguageCode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGetLanguageParam(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"-lang", "zh-CN"}, "zh-CN"},
		{[]string{"status", "-lang=en-US"}, "en-US"},
		{[]string{"/lang", "en-US"}, "en-US"},
		{[]string{"status"}, ""},
		{[]string{"-lang"}, ""},
	}
	for _, tt := range tests {
		if got := GetLanguageParam(tt.args); got != tt.want {
			t.Errorf("GetLanguageParam(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestParseIntoOverlaysValues(t *testing.T) {
	values := map[string]string{"MainForm.Existing": "keep"}
	parseInto("[MainForm]\nExisting=override\nNew=value\\nnext\n", values)

	if got := values["MainForm.Existing"]; got != "override" {
		t.Errorf("Existing = %q, want override", got)
	}
	if got := values["MainForm.New"]; got != "value\r\nnext" {
		t.Errorf("New = %q, want escaped newline", got)
	}
}

func TestLoadLanguageUsesEnglishFallback(t *testing.T) {
	withLanguageState(t)

	external := func(code string) (string, bool) {
		switch code {
		case "en-US":
			return "[Meta]\nFallbackOnly=english\nShared=english\n", true
		case "zh-CN":
			return "[Meta]\nShared=中文\n", true
		default:
			return "", false
		}
	}
	if !loadLanguageWithReader("zh-CN", external) {
		t.Fatal("loadLanguageWithReader returned false")
	}
	if got := L("Meta", "FallbackOnly", ""); got != "english" {
		t.Errorf("FallbackOnly = %q, want english", got)
	}
	if got := L("Meta", "Shared", ""); got != "中文" {
		t.Errorf("Shared = %q, want 中文", got)
	}
}

func TestLanguageSnapshotsStayIndependent(t *testing.T) {
	withLanguageState(t)
	reader := func(code string) (string, bool) {
		return "[Meta]\nNativeName=" + code + "\nSnapshot=" + code + "\n", true
	}
	if !loadLanguageWithReader("en-US", reader) {
		t.Fatal("load English")
	}
	english := current.Load()
	if !loadLanguageWithReader("zh-CN", func(code string) (string, bool) {
		// Publication must wait until every overlay has been read and parsed.
		if current.Load() != english {
			t.Error("published a partially built language")
		}
		return reader(code)
	}) {
		t.Fatal("load Chinese")
	}
	chinese := current.Load()
	if english == chinese || english.code != "en-US" || english.name != "en-US" || english.values["Meta.Snapshot"] != "en-US" {
		t.Fatal("previous snapshot was changed")
	}
	if chinese.code != "zh-CN" || chinese.name != "zh-CN" || chinese.values["Meta.Snapshot"] != "zh-CN" {
		t.Fatal("new snapshot is incomplete")
	}
}

func TestConcurrentLanguageSwitchAndLookup(t *testing.T) {
	withLanguageState(t)
	reader := func(code string) (string, bool) {
		return "[Meta]\nNativeName=" + code + "\nSnapshot=" + code + "\n", true
	}
	loadLanguageWithReader("en-US", reader)
	var wg sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				if worker < 2 {
					code := "en-US"
					if (i+worker)%2 == 0 {
						code = "zh-CN"
					}
					if !loadLanguageWithReader(code, reader) {
						t.Error("language load failed")
						return
					}
				}
				s := current.Load()
				if s.code != s.name || s.code != s.values["Meta.Snapshot"] {
					t.Error("inconsistent language snapshot")
					return
				}
				if got := L("Meta", "Snapshot", "missing"); got != "en-US" && got != "zh-CN" {
					t.Errorf("incomplete translation: %q", got)
				}
				if !Has("Meta", "Snapshot") {
					t.Error("missing snapshot key")
				}
				_ = LF("Missing", "Key", "%s", "fallback")
				_ = CurrentLanguage()
				_ = CurrentLanguageFont()
			}
		}(worker)
	}
	wg.Wait()
}

func TestExternalPartialTargetKeepsEmbeddedTarget(t *testing.T) {
	withLanguageState(t)

	external := func(code string) (string, bool) {
		if code == "zh-CN" {
			return "[Meta]\nExternalOnly=1\n", true
		}
		return "", false
	}
	if !loadLanguageWithReader("zh-CN", external) {
		t.Fatal("loadLanguageWithReader returned false")
	}
	if got := L("Meta", "ExternalOnly", ""); got != "1" {
		t.Errorf("ExternalOnly = %q, want 1", got)
	}
	if got := L("MainForm", "bOK.Caption", ""); got == "" {
		t.Error("embedded zh-CN key was lost after a partial external override")
	}
}

func TestUpdateNoticeInPublishedLanguages(t *testing.T) {
	for _, code := range []string{"en-US", "zh-CN"} {
		values := map[string]string{}
		parseInto(string(assets.Lang(code)), values)
		if values["RdpMux.UpdateNotice"] == "" {
			t.Errorf("%s: missing RdpMux.UpdateNotice", code)
		}
	}
}
