package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"

	"rdpmux/internal/assets"
	"rdpmux/internal/loc"
	"rdpmux/internal/logx"
	"rdpmux/internal/patcher"
	"rdpmux/internal/sysconfig"
	"rdpmux/internal/winsvc"
	"rdpmux/internal/winutil"
)

func tr(key, def string, args ...any) string {
	return loc.LF("Installer", key, def, args...)
}

func ExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func LogFilePath() string {
	return filepath.Join(ExeDir(), "rdpmux.log")
}

func NewLogger(level logx.Level, console io.Writer) (*logx.Logger, func()) {
	return newLogger(level, console, LogFilePath(), os.Stderr)
}

func newLogger(level logx.Level, console io.Writer, path string, stderr io.Writer) (*logx.Logger, func()) {
	var writers []io.Writer
	if console != nil {
		writers = append(writers, console)
	}
	closeLog := func() {}
	if rf, err := logx.NewRotatingFile(path, 5<<20); err == nil {
		writers = append(writers, rf)
		closeLog = rf.Close
	} else {
		diagnostic := console
		if diagnostic == nil {
			diagnostic = stderr
		}
		fmt.Fprintln(diagnostic, tr("LogOpenFailed", "cannot open log file: %s", err))
	}
	return logx.New(level, writers...), closeLog
}

func patchDBPath() string {
	if _, err := winsvc.State(); err == nil {
		dir := sysconfig.InstallDir()
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return filepath.Join(dir, "rdpwrap.ini")
		}
	}
	return winsvc.IniPath()
}

var dbCache struct {
	mu  sync.Mutex
	key string
	db  *patcher.DB
	err error
}

func loadDB() (*patcher.DB, error) {
	path := patchDBPath()
	key := path + "|embedded"
	data := assets.ConfigINI
	if fi, err := os.Stat(path); err == nil {
		key = fmt.Sprintf("%s|%d|%d", path, fi.ModTime().UnixNano(), fi.Size())
		if b, err := os.ReadFile(path); err == nil {
			data = b
		} else {
			key = path + "|embedded"
		}
	}

	dbCache.mu.Lock()
	defer dbCache.mu.Unlock()
	if dbCache.key == key {
		return dbCache.db, dbCache.err
	}
	db, err := patcher.LoadDB(data, sysconfig.Arch())
	dbCache.key, dbCache.db, dbCache.err = key, db, err
	return db, err
}

func beginOperation() (func(), error) {
	name, _ := windows.UTF16PtrFromString(`Global\RdpMux.Management`)
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, err
	}
	runtime.LockOSThread()
	state, err := windows.WaitForSingleObject(h, 0)
	if err != nil || state != windows.WAIT_OBJECT_0 && state != windows.WAIT_ABANDONED {
		runtime.UnlockOSThread()
		windows.CloseHandle(h)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("another RdpMux operation is already running")
	}
	return func() { windows.ReleaseMutex(h); runtime.UnlockOSThread(); windows.CloseHandle(h) }, nil
}

func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".rdpmux-*.tmp")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(temp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, data)
}

func deployFiles(log *logx.Logger) (string, error) {
	dir := sysconfig.InstallDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	exeDir := filepath.Dir(exe)
	cliSrc := filepath.Join(exeDir, "rdpmux.exe")
	if strings.EqualFold(filepath.Base(exe), "rdpmux.exe") {
		cliSrc = exe
	} else if _, err := os.Stat(cliSrc); err != nil {
		return "", fmt.Errorf("%s", tr("ErrCliNotFound", "rdpmux.exe not found next to %s", filepath.Base(exe)))
	}
	target := filepath.Join(dir, "rdpmux.exe")
	if !strings.EqualFold(cliSrc, target) {
		if err := copyFile(cliSrc, target); err != nil {
			return "", err
		}
	}
	guiSrc := filepath.Join(exeDir, "rdpmuxgui.exe")
	if _, err := os.Stat(guiSrc); err == nil {
		if guiTarget := filepath.Join(dir, "rdpmuxgui.exe"); !strings.EqualFold(guiSrc, guiTarget) {
			if err := copyFile(guiSrc, guiTarget); err != nil {
				return "", err
			}
		}
	}
	iniPath := filepath.Join(dir, "rdpwrap.ini")
	if _, err := os.Stat(iniPath); err != nil {
		if err := writeFileAtomic(iniPath, assets.ConfigINI); err != nil {
			return "", err
		}
	}
	confPath := filepath.Join(dir, "rdpmux.conf")
	if _, err := os.Stat(confPath); err != nil {
		conf := "# RdpMux configuration\r\n# log-level: error, warn, info, debug, trace\r\nlog-level=info\r\n"
		if err := writeFileAtomic(confPath, []byte(conf)); err != nil {
			return "", err
		}
	}
	log.Info("%s", tr("LogFilesDeployed", "files deployed to %s", dir))
	return target, nil
}

func pendingDeletion(entries []string, dir string) bool {
	for i := 0; i+1 < len(entries); i += 2 {
		if entries[i+1] != "" {
			continue
		}
		path := strings.TrimPrefix(entries[i], `\??\`)
		if strings.EqualFold(filepath.Clean(filepath.Dir(path)), filepath.Clean(dir)) {
			return true
		}
	}
	return false
}

func checkPendingDeletion(dir string) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager`, registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	entries, _, err := key.GetStringsValue("PendingFileRenameOperations")
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return err
	}
	if pendingDeletion(entries, dir) {
		return fmt.Errorf("RdpMux files are pending deletion; restart Windows before reinstalling")
	}
	return nil
}

func Install(log *logx.Logger) (err error) {
	unlock, err := beginOperation()
	if err != nil {
		return err
	}
	defer unlock()
	if _, stateErr := winsvc.State(); stateErr == nil {
		return fmt.Errorf("%s", tr("SvcAlreadyInstalled", "service %s already installed", winsvc.Name))
	} else if !errors.Is(stateErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return stateErr
	}
	if err := checkPendingDeletion(sysconfig.InstallDir()); err != nil {
		return err
	}
	exePath, err := deployFiles(log)
	if err != nil {
		return err
	}
	log.Info("%s", tr("LogInstallingService", "installing patch service..."))
	if err = winsvc.Install(exePath); err != nil {
		return err
	}
	var undo func() error
	var restoreStart func() error
	termStarted := false
	defer func() {
		if err == nil {
			return
		}
		err = errors.Join(err, winsvc.Remove())
		if termStarted {
			err = errors.Join(err, winutil.SvcStop("TermService"))
		}
		if restoreStart != nil {
			err = errors.Join(err, restoreStart())
		}
		if undo != nil {
			err = errors.Join(err, undo())
		}
	}()
	log.Info("%s", tr("LogEnablingRDP", "enabling Remote Desktop..."))
	undo, err = sysconfig.EnableRDP()
	if err != nil {
		return err
	}
	state, _, err := sysconfig.TermServiceState()
	if err != nil {
		return err
	}
	if state != uint32(svc.Running) {
		start, startErr := winutil.SvcGetStart("TermService")
		if startErr != nil {
			return startErr
		}
		if start == windows.SERVICE_DISABLED {
			if err = winutil.SvcSetStart("TermService", windows.SERVICE_AUTO_START); err != nil {
				return err
			}
			restoreStart = func() error { return winutil.SvcSetStart("TermService", start) }
		}
		if err = winutil.SvcStart("TermService"); err != nil {
			return err
		}
		termStarted = true
	}
	if err = winsvc.Start(); err != nil {
		return err
	}
	if dropErr := sysconfig.DropRfxvmt(); dropErr != nil {
		log.Warn("%s", tr("LogRfxvmtWarn", "could not write rfxvmt.dll: %s", dropErr))
	}
	log.Info("%s", tr("LogInstalled", "installed; the service maintains concurrent sessions"))
	return nil
}

func Uninstall(keep bool, log *logx.Logger) error {
	unlock, err := beginOperation()
	if err != nil {
		return err
	}
	defer unlock()
	log.Info("%s", tr("LogRemovingService", "removing patch service..."))
	if err := winsvc.Remove(); err != nil {
		return err
	}
	log.Info("%s", tr("LogRestartingTermService", "restarting TermService to clear patches (active RDP sessions will disconnect)..."))
	if err := sysconfig.RestartTermService(); err != nil {
		return err
	}
	if !keep {
		log.Info("%s", tr("LogRestoringRDP", "disabling Remote Desktop and removing RdpMux firewall rules..."))
		if err := sysconfig.DisableRDP(); err != nil {
			return err
		}
	}
	dir := sysconfig.InstallDir()
	var cleanupErr error
	for _, name := range []string{"rdpmux.exe", "rdpmuxgui.exe", "rdpwrap.ini", "rdpmux.log", "rdpmux.log.1", "rdpmux.conf"} {
		p := filepath.Join(dir, name)
		err := os.Remove(p)
		if err == nil || os.IsNotExist(err) {
			continue
		}
		if derr := winutil.DeleteFileOnReboot(p); derr == nil {
			log.Info("%s", tr("LogDeleteOnReboot", "%s is locked; it will be removed after the next reboot", name))
		} else {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove %s: %w", name, errors.Join(err, derr)))
		}
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) && !errors.Is(err, windows.ERROR_DIR_NOT_EMPTY) {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	log.Info("%s", tr("LogUninstalled", "uninstalled; termsrv.dll is back to stock behavior"))
	return nil
}

func Update(source string, log *logx.Logger) error {
	unlock, err := beginOperation()
	if err != nil {
		return err
	}
	defer unlock()
	if source == "" {
		source = patcher.DefaultINIURL
	}
	log.Info("%s", loc.L("RdpMux", "UpdateNotice", "Note: the INI contains unsigned patch bytes written into TermService; update only from a trusted source."))
	sourceLabel := "HTTPS"
	if parsed, parseErr := url.Parse(source); parseErr == nil && parsed.Host != "" {
		sourceLabel = parsed.Host
	}
	log.Info("%s", tr("LogDownloadingDB", "downloading patch database from %s", sourceLabel))
	content, err := patcher.DownloadINI(source)
	if err != nil {
		return err
	}
	db, err := patcher.LoadDB([]byte(content), sysconfig.Arch())
	if err != nil {
		return fmt.Errorf("%s", tr("ErrDownloadedINIInvalid", "downloaded ini invalid: %s", err))
	}
	if len(db.Invalid) > 0 {
		log.Warn("%d invalid version entries will not be used", len(db.Invalid))
	}
	newDate, _ := patcher.INIDate(content)
	iniPath := patchDBPath()
	if err := os.MkdirAll(filepath.Dir(iniPath), 0755); err != nil {
		return err
	}
	old, _ := os.ReadFile(iniPath)
	oldDate, _ := patcher.INIDate(string(old))
	if bytes.Equal(old, []byte(content)) {
		log.Info("%s", tr("LogAlreadyUpToDate", "already up to date (%d)", newDate))
		return nil
	}
	if err := writeFileAtomic(iniPath, []byte(content)); err != nil {
		return err
	}
	log.Info("%s", tr("LogDBUpdated", "patch database updated (%d -> %d)", oldDate, newDate))
	if st, err := winsvc.State(); err == nil && st == svc.Running {
		log.Info("%s", tr("LogRestartingPatchService", "restarting patch service..."))
		if err := winsvc.Restart(); err != nil {
			return err
		}
	}
	return nil
}

func PatchOnce(log *logx.Logger) error {
	unlock, err := beginOperation()
	if err != nil {
		return err
	}
	defer unlock()
	db, err := loadDB()
	if err != nil {
		return err
	}
	w := &patcher.Watcher{DB: db, Arch: sysconfig.Arch(), Log: log}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return w.RunOnce(ctx, 15*time.Second)
}

func RunService() error {
	loc.Init("", nil)
	level := logx.ResolveLevel("", ExeDir())
	rf, err := logx.NewRotatingFile(LogFilePath(), 5<<20)
	if err != nil {
		return err
	}
	defer rf.Close()
	log := logx.New(level, rf)
	log.Info("%s", tr("LogServiceStarting", "service starting (log level=%s)", level))
	if err := winutil.EnablePrivilege("SeDebugPrivilege"); err != nil {
		log.Error("SeDebugPrivilege: %v", err)
		return err
	}
	db, err := loadDB()
	if err != nil {
		log.Error("%s", tr("ErrLoadPatchDB", "load patch db: %s", err))
		return err
	}
	w := &patcher.Watcher{DB: db, Arch: sysconfig.Arch(), Log: log}
	return winsvc.Run(w)
}

type Status struct {
	TermsrvVersion   string
	TermsrvErr       error
	DBUpdated        string
	DBErr            error
	Supported        bool
	TermState        uint32
	TermPID          uint32
	TermErr          error
	PatchActive      bool
	PatchNeedAdmin   bool
	PatchErr         error
	Listener         bool
	ServiceState     svc.State
	ServiceInstalled bool
}

func GetStatus() Status {
	var s Status
	fv, err := winutil.GetFileVersion(winutil.ExpandSystemPath("System32", "termsrv.dll"))
	if err != nil {
		s.TermsrvErr = err
	} else {
		s.TermsrvVersion = fv.String()
	}
	db, err := loadDB()
	if err != nil {
		s.DBErr = err
	} else {
		s.DBUpdated = db.Updated
		if s.TermsrvErr == nil {
			_, s.Supported = db.Spec(s.TermsrvVersion)
		}
	}
	s.TermState, s.TermPID, s.TermErr = sysconfig.TermServiceState()
	if s.TermErr == nil && s.TermState == 4 && db != nil {
		if !winutil.IsElevated() {
			s.PatchNeedAdmin = true
		} else if p, err := patcher.New(s.TermPID, db, sysconfig.Arch(), nil); err != nil {
			s.PatchErr = err
		} else {
			s.PatchActive, s.PatchErr = p.Verify()
			p.Close()
		}
	}
	s.Listener = sysconfig.IsListenerWorking()
	if st, err := winsvc.State(); err == nil {
		s.ServiceInstalled = true
		s.ServiceState = st
	}
	return s
}
