package loc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"rdpmux/internal/assets"
)

const DefaultLanguage = "en-US"
const languageRegKey = `Software\RDP Wrapper`

type LanguageInfo struct {
	Code       string
	NativeName string
}

var SupportedLanguages = []LanguageInfo{
	{"en-US", "English"},
	{"zh-CN", "简体中文"},
}

// Published language snapshots, including their maps, are immutable.
type languageState struct {
	values map[string]string
	code   string
	name   string
}

var current atomic.Pointer[languageState]

func init() {
	current.Store(&languageState{code: DefaultLanguage, name: "English"})
}

func NormalizeLanguageCode(code string) string {
	s := strings.ToLower(strings.TrimSpace(code))
	if strings.HasPrefix(s, "zh") {
		return "zh-CN"
	}
	if strings.HasPrefix(s, "en") {
		return "en-US"
	}
	return ""
}

func GetLanguageParam(args []string) string {
	for i, p := range args {
		lower := strings.ToLower(p)
		if lower == "-lang" || lower == "/lang" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(lower, "-lang=") || strings.HasPrefix(lower, "/lang=") {
			return p[6:]
		}
	}
	return ""
}

var getUserDefaultUILanguage = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetUserDefaultUILanguage")

func systemLanguageCode() string {
	langID, _, _ := getUserDefaultUILanguage.Call()
	switch langID & 0x3FF {
	case 4:
		return "zh-CN"
	case 9:
		return "en-US"
	}
	return ""
}

func readSavedLanguage() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, languageRegKey, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	s, _, err := k.GetStringValue("Language")
	if err != nil {
		return ""
	}
	return s
}

func saveLanguage(code string) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, languageRegKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.SetStringValue("Language", code)
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i == len(s)-1 {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case '\\':
			b.WriteByte('\\')
		case 'n':
			b.WriteString("\r\n")
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 's':
			b.WriteByte(' ')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func parseInto(text string, dst map[string]string) {
	section := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed[0] == ';' || trimmed[0] == '#' {
			continue
		}
		if trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']' {
			section = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			continue
		}
		if section == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimRight(key, " \t")
		value = strings.TrimPrefix(value, " ")
		if key == "" {
			continue
		}
		dst[section+"."+key] = unescape(value)
	}
}

func readExternalLanguage(code string) (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "lang", code+".ini"))
	if err != nil {
		return "", false
	}
	return string(data), true
}

func loadLanguageWithReader(code string, read func(string) (string, bool)) bool {
	code = NormalizeLanguageCode(code)
	if code == "" {
		code = DefaultLanguage
	}
	base := string(assets.Lang(DefaultLanguage))
	if base == "" {
		return false
	}
	values := make(map[string]string)
	parseInto(base, values)
	if text, ok := read(DefaultLanguage); ok {
		parseInto(text, values)
	}
	if code != DefaultLanguage {
		target := string(assets.Lang(code))
		if target == "" {
			return loadLanguageWithReader(DefaultLanguage, read)
		}
		parseInto(target, values)
		if text, ok := read(code); ok {
			parseInto(text, values)
		}
	}
	name, ok := values["Meta.NativeName"]
	if !ok {
		name = code
	}
	current.Store(&languageState{values: values, code: code, name: name})
	return true
}

func loadLanguage(code string) bool {
	return loadLanguageWithReader(code, readExternalLanguage)
}

func Init(override string, args []string) bool {
	code := NormalizeLanguageCode(override)
	if code == "" {
		code = NormalizeLanguageCode(GetLanguageParam(args))
	}
	if code == "" {
		code = NormalizeLanguageCode(readSavedLanguage())
	}
	if code == "" {
		code = NormalizeLanguageCode(systemLanguageCode())
	}
	if code == "" {
		code = DefaultLanguage
	}
	return loadLanguage(code)
}

func CurrentLanguage() string     { return current.Load().code }
func CurrentLanguageFont() string { return L("Meta", "FontName", "") }

func L(section, key, def string) string {
	if v, ok := current.Load().values[section+"."+key]; ok {
		return v
	}
	return def
}

func LF(section, key, def string, args ...any) string {
	s := fmt.Sprintf(L(section, key, def), args...)
	if strings.Contains(s, "%!") {
		s = fmt.Sprintf(def, args...)
		if strings.Contains(s, "%!") {
			return def
		}
	}
	return s
}

func Has(section, key string) bool {
	_, ok := current.Load().values[section+"."+key]
	return ok
}

func SetCurrent(code string, persist bool) bool {
	normalized := NormalizeLanguageCode(code)
	if normalized == "" || !loadLanguage(normalized) {
		return false
	}
	if persist {
		saveLanguage(normalized)
	}
	return true
}
