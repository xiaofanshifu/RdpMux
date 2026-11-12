package main

import (
	"reflect"
	"testing"

	"rdpmux/internal/loc"
)

func TestParseFlagsRejectsUnsafeArguments(t *testing.T) {
	for _, args := range [][]string{
		{"uninstall", "-lang", "-k"}, {"uninstall", "--unknown"}, {"uninstall", "-k", "extra"},
		{"install", "extra"}, {"patch", "--unknown"}, {"status", "-log-level"},
		{"status", "-log-level="}, {"status", "-log-level", "wrong"}, {"status", "-lang="},
		{"update", "http://example.invalid/patch.ini"}, {"update", "https://example.invalid/patch.ini", "extra"},
	} {
		if _, _, err := parseFlags(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestParseFlagsHelpCannotUninstall(t *testing.T) {
	_, args, err := parseFlags([]string{"uninstall", "--help"})
	if err != nil || !reflect.DeepEqual(args, []string{"help"}) {
		t.Fatalf("%v %v", args, err)
	}
	if needsElevation(args[0]) {
		t.Fatal("help requires elevation")
	}
}

func TestParseFlagsKeepsUninstallOption(t *testing.T) {
	_, args, err := parseFlags([]string{"-lang", "zh-CN", "uninstall", "-v", "-k"})
	if err != nil || !reflect.DeepEqual(args, []string{"uninstall", "-k"}) {
		t.Fatalf("%v %v", args, err)
	}
}

func TestParseFlagsChineseErrorsAndHelp(t *testing.T) {
	oldLanguage := loc.CurrentLanguage()
	t.Cleanup(func() {
		if !loc.SetCurrent(oldLanguage, false) {
			t.Errorf("could not restore language %q", oldLanguage)
		}
	})
	if !loc.SetCurrent("zh-CN", false) {
		t.Fatal("could not select Chinese")
	}

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"missing language", []string{"status", "-lang"}, "-lang 需要指定 en-US 或 zh-CN"},
		{"invalid language", []string{"status", "-lang", "fr-FR"}, "-lang 需要指定 en-US 或 zh-CN"},
		{"language consumes keep option", []string{"uninstall", "-lang", "-k"}, "-lang 需要指定 en-US 或 zh-CN"},
		{"invalid slash language", []string{"status", "/lang", "fr-FR"}, "/lang 需要指定 en-US 或 zh-CN"},
		{"invalid inline language", []string{"status", "-lang=fr-FR"}, "无效的语言：fr-FR"},
		{"invalid inline slash language", []string{"status", "/lang=fr-FR"}, "无效的语言：fr-FR"},
		{"empty language", []string{"status", "-lang="}, "无效的语言："},
		{"missing log level", []string{"status", "-log-level"}, "-log-level 需要指定值"},
		{"invalid log level", []string{"status", "-log-level", "wrong"}, "无效的日志级别：wrong"},
		{"invalid inline log level", []string{"status", "-log-level=wrong"}, "无效的日志级别：wrong"},
		{"empty log level", []string{"status", "-log-level="}, "无效的日志级别："},
		{"unknown command", []string{"UNKNOWN"}, "未知命令：unknown"},
		{"extra install argument", []string{"install", "extra"}, "install 不接受位置参数或额外选项"},
		{"extra status option", []string{"status", "--unknown"}, "status 不接受位置参数或额外选项"},
		{"extra patch option", []string{"patch", "--unknown"}, "patch 不接受位置参数或额外选项"},
		{"extra service argument", []string{"service", "extra"}, "service 不接受位置参数或额外选项"},
		{"extra help argument", []string{"help", "extra"}, "help 不接受位置参数或额外选项"},
		{"invalid uninstall option", []string{"uninstall", "--unknown"}, "uninstall 仅接受 -k 或 --keep"},
		{"extra uninstall argument", []string{"uninstall", "-k", "extra"}, "uninstall 仅接受 -k 或 --keep"},
		{"insecure update URL", []string{"update", "http://example.invalid/patch.ini"}, "update 仅接受一个 HTTPS URL"},
		{"invalid update option", []string{"update", "--unknown"}, "update 仅接受一个 HTTPS URL"},
		{"extra update argument", []string{"update", "https://example.invalid/patch.ini", "extra"}, "update 仅接受一个 HTTPS URL"},
		{"language checked before help", []string{"uninstall", "--help", "-lang=fr-FR"}, "无效的语言：fr-FR"},
		{"missing language before help", []string{"uninstall", "--help", "-lang"}, "-lang 需要指定 en-US 或 zh-CN"},
		{"log level checked before help", []string{"uninstall", "-log-level=wrong", "--help"}, "无效的日志级别：wrong"},
		{"missing log level before help", []string{"uninstall", "--help", "-log-level"}, "-log-level 需要指定值"},
		{"help consumed as log level", []string{"uninstall", "-log-level", "--help"}, "无效的日志级别：--help"},
		{"language error first", []string{"unknown", "-lang=fr-FR", "-log-level=wrong"}, "无效的语言：fr-FR"},
		{"log level error first", []string{"unknown", "-log-level=wrong", "-lang=fr-FR"}, "无效的日志级别：wrong"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, args, err := parseFlags(tt.args)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("parseFlags(%q) error = %v, want %q", tt.args, err, tt.want)
			}
			if args != nil {
				t.Fatalf("invalid arguments returned a command: %q", args)
			}
		})
	}

	t.Run("help takes precedence over command validation", func(t *testing.T) {
		for _, input := range [][]string{
			{"help"}, {"--help"}, {"-h"},
			{"uninstall", "--help"}, {"uninstall", "-k", "-h"},
			{"uninstall", "--unknown", "--help"},
			{"update", "http://example.invalid/patch.ini", "--help"},
			{"install", "extra", "--help"}, {"unknown", "--help"},
			{"--help", "uninstall", "-lang", "zh-CN", "-log-level", "debug"},
		} {
			_, args, err := parseFlags(input)
			if err != nil || !reflect.DeepEqual(args, []string{"help"}) {
				t.Fatalf("parseFlags(%q) = %q, %v; want help", input, args, err)
			}
			if needsElevation(args[0]) {
				t.Fatalf("help requires elevation for %q", input)
			}
		}
	})
}
