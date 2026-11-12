package patcher

import (
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultINIURL = "https://raw.githubusercontent.com/sebaxakerhtc/rdpwrap.ini/master/rdpwrap.ini"

func DownloadINI(source string) (string, error) {
	if source == "" {
		source = DefaultINIURL
	}
	u, err := url.Parse(source)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("patch database source must be an HTTPS URL without credentials")
	}
	client := &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !strings.EqualFold(req.URL.Scheme, "https") || req.URL.User != nil {
				return fmt.Errorf("patch database redirect must use HTTPS without credentials")
			}
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, source, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "RdpMux")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return "", err
	}
	if len(data) > 16<<20 {
		return "", fmt.Errorf("patch database exceeds 16 MiB")
	}
	return string(data), nil
}

func INIDate(content string) (int, bool) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "Updated=") {
			continue
		}
		value := strings.ReplaceAll(strings.TrimPrefix(line, "Updated="), "-", "")
		date, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, false
		}
		return date, true
	}
	return 0, false
}

type CodePatch struct {
	Name   string
	Offset uint32
	Code   []byte
}

type Spec struct {
	LocalOnly     *CodePatch
	SingleUser    *CodePatch
	DefPolicy     *CodePatch
	SLInitGlobals map[string]uint32
}

type DB struct {
	Updated      string
	Specs        map[string]*Spec
	Invalid      map[string]error
	SLInitValues map[string]uint32
}

func parseHexU32(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	v, err := strconv.ParseUint(s, 16, 32)
	return uint32(v), err
}

func parseSections(data string) map[string]map[string]string {
	sections := map[string]map[string]string{}
	current := ""
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == ';' || line[0] == '#' {
			continue
		}
		if line[0] == '[' && line[len(line)-1] == ']' {
			current = strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := sections[current]; !ok {
				sections[current] = map[string]string{}
			}
			continue
		}
		if current == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if i := strings.IndexByte(value, ';'); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		if key != "" {
			sections[current][key] = value
		}
	}
	return sections
}

func LoadDB(data []byte, arch string) (*DB, error) {
	if arch != "x64" {
		return nil, fmt.Errorf("unsupported patch architecture: %s", arch)
	}
	sections := parseSections(strings.TrimPrefix(string(data), "\uFEFF"))
	if _, err := time.Parse("2006-01-02", sections["Main"]["Updated"]); err != nil {
		return nil, fmt.Errorf("missing or invalid Main.Updated: %w", err)
	}
	db := &DB{
		Updated:      sections["Main"]["Updated"],
		Specs:        map[string]*Spec{},
		Invalid:      map[string]error{},
		SLInitValues: map[string]uint32{},
	}
	for name, value := range sections["SLInit"] {
		v, err := parseHexU32(value)
		if err != nil {
			return nil, fmt.Errorf("SLInit.%s: %w", name, err)
		}
		db.SLInitValues[name] = v
	}
	if len(db.SLInitValues) != len(slInitOrder) {
		return nil, fmt.Errorf("SLInit must contain exactly the %d required globals", len(slInitOrder))
	}
	for _, name := range slInitOrder {
		v, ok := db.SLInitValues[name]
		if !ok {
			return nil, fmt.Errorf("SLInit: missing %s", name)
		}
		if strings.HasPrefix(name, "b") && v > 1 {
			return nil, fmt.Errorf("SLInit.%s must be 0 or 1", name)
		}
	}
	if db.SLInitValues["bInitialized"] != 1 {
		return nil, fmt.Errorf("SLInit.bInitialized must be 1")
	}
	patchCodes := map[string][]byte{}
	for name, value := range sections["PatchCodes"] {
		code, err := hex.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("PatchCodes.%s: %w", name, err)
		}
		patchCodes[name] = code
	}

	buildCodePatch := func(sec map[string]string, prefix string) (*CodePatch, error) {
		if sec[prefix+"Patch."+arch] != "1" {
			return nil, nil
		}
		offset, err := parseHexU32(sec[prefix+"Offset."+arch])
		if err != nil {
			return nil, fmt.Errorf("%sOffset: %w", prefix, err)
		}
		code, ok := patchCodes[sec[prefix+"Code."+arch]]
		if !ok || len(code) == 0 {
			return nil, fmt.Errorf("%sCode: unknown patch code %q", prefix, sec[prefix+"Code."+arch])
		}
		return &CodePatch{Name: prefix, Offset: offset, Code: code}, nil
	}

versions:
	for name, sec := range sections {
		if strings.HasSuffix(name, "-SLInit") || strings.Contains(name, " ") {
			continue
		}
		if !strings.HasPrefix(name, "10.0.") {
			continue
		}
		parts := strings.Split(name, ".")
		if len(parts) != 4 {
			return nil, fmt.Errorf("invalid version section %q", name)
		}
		for _, part := range parts {
			if _, err := strconv.ParseUint(part, 10, 16); err != nil {
				return nil, fmt.Errorf("invalid version section %q", name)
			}
		}
		if sec["SLPolicyInternal."+arch] == "1" {
			continue
		}
		spec := &Spec{}
		var err error
		if spec.LocalOnly, err = buildCodePatch(sec, "LocalOnly"); err != nil {
			db.Invalid[name] = err
			continue versions
		}
		if spec.SingleUser, err = buildCodePatch(sec, "SingleUser"); err != nil {
			db.Invalid[name] = err
			continue versions
		}
		if spec.DefPolicy, err = buildCodePatch(sec, "DefPolicy"); err != nil {
			db.Invalid[name] = err
			continue versions
		}
		if sec["SLInitHook."+arch] == "1" {
			initSec, ok := sections[name+"-SLInit"]
			if !ok {
				db.Invalid[name] = fmt.Errorf("missing -SLInit section")
				continue versions
			}
			spec.SLInitGlobals = map[string]uint32{}
			for key, value := range initSec {
				gname, garch, ok := strings.Cut(key, ".")
				if !ok || garch != arch {
					continue
				}
				rva, err := parseHexU32(value)
				if err != nil {
					db.Invalid[name] = fmt.Errorf("%s: %w", key, err)
					continue versions
				}
				spec.SLInitGlobals[gname] = rva
			}
			if len(spec.SLInitGlobals) != len(slInitOrder) {
				db.Invalid[name] = fmt.Errorf("expected %d SLInit globals", len(slInitOrder))
				continue versions
			}
			seen := map[uint32]bool{}
			for _, key := range slInitOrder {
				rva, ok := spec.SLInitGlobals[key]
				if !ok || rva == 0 || rva%4 != 0 || seen[rva] {
					db.Invalid[name] = fmt.Errorf("missing, unaligned or duplicate %s", key)
					continue versions
				}
				seen[rva] = true
			}
		}
		if spec.LocalOnly == nil && spec.SingleUser == nil && spec.DefPolicy == nil && spec.SLInitGlobals == nil {
			continue
		}
		db.Specs[name] = spec
	}
	if len(db.Specs) == 0 {
		return nil, fmt.Errorf("patch database contains no supported Windows 10/11 x64 versions")
	}
	return db, nil
}

func (db *DB) Spec(version string) (*Spec, bool) {
	spec, ok := db.Specs[version]
	return spec, ok
}
