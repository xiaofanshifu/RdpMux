package main

//go:generate go run github.com/akavel/rsrc -manifest rdpmux.manifest -o rsrc_windows_amd64.syso

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows/svc"

	"rdpmux/internal/app"
	"rdpmux/internal/loc"
	"rdpmux/internal/logx"
	"rdpmux/internal/winutil"
)

func out(format string, args ...any) {
	winutil.ConsoleWln(fmt.Sprintf(format, args...))
}

func tr(key, def string, args ...any) string {
	return loc.LF("RdpMux", key, def, args...)
}

func usage() {
	out("%s", tr("UsageTitle", "RdpMux - concurrent RDP sessions for Windows client editions"))
	out("")
	out("%s", tr("UsageLine", "Usage: rdpmux <command> [options]"))
	out("")
	out("  install        %s", tr("CmdInstall", "enable RDP, deploy files and start the patch service"))
	out("  uninstall [-k] %s", tr("CmdUninstall", "remove service and restore stock termsrv (-k keeps RDP settings)"))
	out("  status         %s", tr("CmdStatus", "show version, support, service and patch state"))
	out("  update [url]   %s", tr("CmdUpdate", "download latest patch database (rdpwrap.ini)"))
	out("                 %s", tr("UpdateNotice", "Note: the INI contains unsigned patch bytes written into TermService; update only from a trusted source."))
	out("  patch          %s", tr("CmdPatch", "apply patches once now (without the service)"))
	out("  service        %s", tr("CmdService", "run as Windows service (internal)"))
	out("  help           %s", tr("CmdHelp", "show this help"))
	out("")
	out("  %s", tr("UsageLang", "-lang <en-US|zh-CN>  set interface language for this run"))
	out("%s", tr("UsageLogging", "Logging: -v (debug), -vv (trace), -log-level <error|warn|info|debug|trace>"))
	out("%s", tr("UsageLoggingConfig", "         or set log-level=debug in rdpmux.conf next to the exe, or env RDPMUX_LOG_LEVEL"))
	out("%s", tr("UsageLogFile", "         log file: rdpmux.log next to the exe (rotated, keeps .1 backup)"))
}

func termStateText(state uint32) string {
	switch state {
	case 1:
		return tr("Stopped", "Stopped")
	case 2:
		return tr("Starting", "Starting...")
	case 3:
		return tr("Stopping", "Stopping...")
	case 4:
		return tr("Running", "Running")
	case 5:
		return tr("Resuming", "Resuming...")
	case 6:
		return tr("Suspending", "Suspending...")
	case 7:
		return tr("Suspended", "Suspended")
	default:
		return tr("Unknown", "Unknown")
	}
}

func svcStateText(st svc.State) string {
	switch st {
	case svc.Stopped:
		return tr("Stopped", "Stopped")
	case svc.StartPending:
		return tr("Starting", "Starting...")
	case svc.StopPending:
		return tr("Stopping", "Stopping...")
	case svc.Running:
		return tr("Running", "Running")
	default:
		return tr("Unknown", "Unknown")
	}
}

func cmdStatus(log *logx.Logger) {
	s := app.GetStatus()
	if s.TermsrvErr != nil {
		out("%s", tr("TermsrvLine", "termsrv.dll: %s", s.TermsrvErr))
	} else {
		out("%s", tr("TermsrvLine", "termsrv.dll: %s", s.TermsrvVersion))
	}
	log.Info("%s", tr("LogStatus", "status: termsrv=%s supported=%v termstate=%d termpid=%d patch=%v listener=%v service=%v",
		s.TermsrvVersion, s.Supported, s.TermState, s.TermPID, s.PatchActive, s.Listener, s.ServiceInstalled))
	if s.DBErr != nil {
		out("%s", tr("DBError", "patch db: %s", s.DBErr))
	} else {
		out("%s", tr("DBDate", "patch db updated: %s", s.DBUpdated))
		if s.TermsrvErr == nil {
			if s.Supported {
				out("%s", tr("Supported", "support: fully supported"))
			} else {
				out("%s", tr("NotSupported", "support: NOT supported (run 'rdpmux update')"))
			}
		}
	}
	if s.TermErr != nil {
		out("%s", tr("TermServiceLine", "TermService: %s", s.TermErr))
	} else {
		out("%s", tr("TermServicePID", "TermService: %s (pid %d)", termStateText(s.TermState), s.TermPID))
		if s.TermState == 4 {
			switch {
			case s.PatchNeedAdmin:
				out("%s", tr("PatchNeedAdmin", "patches: run as administrator to check"))
			case s.PatchErr != nil:
				out("%s", tr("PatchState", "patches: %s", s.PatchErr))
			case s.PatchActive:
				out("%s", tr("Patched", "patches: ACTIVE"))
			default:
				out("%s", tr("NotPatched", "patches: inactive"))
			}
		}
	}
	if s.Listener {
		out("%s", tr("Listening", "listener: Listening"))
	} else {
		out("%s", tr("NotListening", "listener: Not listening"))
	}
	if !s.ServiceInstalled {
		out("%s", tr("WatcherAbsent", "patch service: not installed"))
	} else {
		out("%s", tr("ServiceLine", "patch service: %s", svcStateText(s.ServiceState)))
	}
	out("%s", tr("LogInfo", "log: %s (level=%s)", app.LogFilePath(), log.Level()))
}

func main() {
	if err := run(); err != nil {
		out("[-] %s", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (logx.Level, []string, error) {
	levelFlag := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		lower := strings.ToLower(a)
		switch {
		case lower == "-lang" || lower == "/lang":
			if i+1 >= len(args) || loc.NormalizeLanguageCode(args[i+1]) == "" {
				return 0, nil, fmt.Errorf("%s", tr("ErrLanguageRequired", "%s requires en-US or zh-CN", a))
			}
			i++
		case strings.HasPrefix(lower, "-lang=") || strings.HasPrefix(lower, "/lang="):
			if loc.NormalizeLanguageCode(a[6:]) == "" {
				return 0, nil, fmt.Errorf("%s", tr("ErrInvalidLanguage", "invalid language: %s", a[6:]))
			}
		case a == "-v":
			levelFlag = "debug"
		case a == "-vv":
			levelFlag = "trace"
		case a == "-log-level":
			if i+1 >= len(args) {
				return 0, nil, fmt.Errorf("%s", tr("ErrLogLevelRequired", "-log-level requires a value"))
			}
			i++
			levelFlag = args[i]
			if _, ok := logx.ParseLevel(levelFlag); !ok {
				return 0, nil, fmt.Errorf("%s", tr("ErrInvalidLogLevel", "invalid log level: %s", levelFlag))
			}
		case strings.HasPrefix(a, "-log-level="):
			levelFlag = strings.TrimPrefix(a, "-log-level=")
			if _, ok := logx.ParseLevel(levelFlag); !ok {
				return 0, nil, fmt.Errorf("%s", tr("ErrInvalidLogLevel", "invalid log level: %s", levelFlag))
			}
		default:
			rest = append(rest, a)
		}
	}
	for _, a := range rest {
		if a == "-h" || a == "--help" {
			return logx.ResolveLevel(levelFlag, app.ExeDir()), []string{"help"}, nil
		}
	}
	if len(rest) > 0 {
		rest[0] = strings.ToLower(rest[0])
		switch rest[0] {
		case "install", "status", "patch", "service", "help":
			if len(rest) != 1 {
				return 0, nil, fmt.Errorf("%s", tr("ErrUnexpectedArguments", "%s does not accept positional arguments or extra options", rest[0]))
			}
		case "uninstall":
			if len(rest) > 2 || len(rest) == 2 && rest[1] != "-k" && rest[1] != "--keep" {
				return 0, nil, fmt.Errorf("%s", tr("ErrUninstallArguments", "uninstall accepts only -k or --keep"))
			}
		case "update":
			if len(rest) > 2 || len(rest) == 2 && !strings.HasPrefix(strings.ToLower(rest[1]), "https://") {
				return 0, nil, fmt.Errorf("%s", tr("ErrUpdateArguments", "update accepts one HTTPS URL"))
			}
		default:
			return 0, nil, fmt.Errorf("%s", tr("ErrUnknownCommand", "unknown command: %s", rest[0]))
		}
	}
	return logx.ResolveLevel(levelFlag, app.ExeDir()), rest, nil
}

func needsElevation(cmd string) bool {
	switch cmd {
	case "install", "uninstall", "update", "patch":
		return true
	}
	return false
}

func elevate(args []string) error {
	code, err := winutil.RunElevated(args)
	if errors.Is(err, winutil.ErrElevationCancelled) {
		return fmt.Errorf("%s", tr("ErrElevationCancelled", "administrator privileges are required; elevation was cancelled"))
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s", tr("ErrElevatedExit", "elevated command failed with exit code %d", code))
	}
	return nil
}

func run() error {
	args := os.Args[1:]
	loc.Init("", args)
	level, args, err := parseFlags(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		usage()
		return nil
	}
	cmd := strings.ToLower(args[0])
	if needsElevation(cmd) && !winutil.IsElevated() {
		return elevate(os.Args[1:])
	}
	if err := winutil.EnablePrivilege("SeDebugPrivilege"); err != nil && cmd == "patch" {
		return err
	}
	switch cmd {
	case "install":
		log, closeLog := app.NewLogger(level, os.Stdout)
		defer closeLog()
		return app.Install(log)
	case "uninstall":
		keep := len(args) > 1 && (args[1] == "-k" || args[1] == "--keep")
		log, closeLog := app.NewLogger(level, os.Stdout)
		defer closeLog()
		return app.Uninstall(keep, log)
	case "status":
		log, closeLog := app.NewLogger(level, nil)
		defer closeLog()
		cmdStatus(log)
		return nil
	case "update":
		source := ""
		if len(args) > 1 {
			source = args[1]
		}
		log, closeLog := app.NewLogger(level, os.Stdout)
		defer closeLog()
		return app.Update(source, log)
	case "patch":
		log, closeLog := app.NewLogger(level, os.Stdout)
		defer closeLog()
		return app.PatchOnce(log)
	case "service":
		return app.RunService()
	default:
		usage()
		return nil
	}
}
