package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"

	"rdpmux/internal/logx"
	"rdpmux/internal/winsvc"
)

// Operations that change service state or can terminate the calling session.
// They run in a temporary SYSTEM service: stopping TermService ends the
// caller's RDP session, and a process in that session dies before it can finish
// the work.
const (
	OpInstall        = "install"
	OpUninstall      = "uninstall"
	OpRestartService = "restart-service"
	OpRestartTerm    = "restart-term-service"
)

const (
	operationService = "RdpMuxOperation"
	// OperationCommand is the hidden service entry point.
	OperationCommand = "__op-host"
	operationTimeout = 4 * time.Minute
)

// RunOperation executes op in a temporary service so the work survives this
// session being terminated, and streams the service log to display.
func RunOperation(op string, level logx.Level, display io.Writer) error {
	base := filepath.Join(os.TempDir(), fmt.Sprintf("rdpmux-op-%d", os.Getpid()))
	logPath, resultPath := base+".log", base+".result"
	_ = os.Remove(logPath)
	_ = os.Remove(resultPath)
	if err := os.WriteFile(logPath, nil, 0600); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cleanup, err := winsvc.StartOneShot(operationService, exe, OperationCommand, op, level.String(), logPath, resultPath)
	if err != nil {
		return fmt.Errorf("%s: %w", tr("ErrOperationHost", "cannot start the operation host"), err)
	}
	defer cleanup()
	complete, err := followOperation(display, logPath, resultPath, operationTimeout)
	if complete {
		_ = os.Remove(logPath)
		_ = os.Remove(resultPath)
	}
	return err
}

func followOperation(display io.Writer, logPath, resultPath string, timeout time.Duration) (bool, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 4096)
	var pending []byte
	for {
		n, _ := f.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				_, _ = display.Write(pending[:i+1])
				pending = pending[i+1:]
			}
		}
		if data, err := os.ReadFile(resultPath); err == nil {
			if len(pending) > 0 {
				_, _ = display.Write(pending)
			}
			return true, operationResult(string(data))
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("%s: %s", tr("ErrOperationTimeout", "the operation did not report a result"), logPath)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func operationResult(data string) error {
	text := strings.TrimSpace(data)
	if text == "ok" {
		return nil
	}
	return errors.New(strings.TrimPrefix(text, "error: "))
}

func operationReport(err error) string {
	if err == nil {
		return "ok\n"
	}
	return "error: " + err.Error() + "\n"
}

// ServeOperationHost runs the hidden host entry point when the process was
// started for it. Every binary that can start the host must call this before
// its normal entry point.
func ServeOperationHost(args []string) (bool, error) {
	if len(args) == 0 || !strings.EqualFold(args[0], OperationCommand) {
		return false, nil
	}
	return true, RunOperationHost(args[1:])
}

// RunOperationHost executes one operation inside the temporary service.
func RunOperationHost(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("%s requires <operation> <level> <log> <result>", OperationCommand)
	}
	level, _ := logx.ParseLevel(args[1])
	return svc.Run(operationService, &operationHandler{
		op: args[0], level: level, logPath: args[2], resultPath: args[3],
	})
}

type operationHandler struct {
	op         string
	level      logx.Level
	logPath    string
	resultPath string
}

func (h *operationHandler) Execute(_ []string, _ <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	changes <- svc.Status{State: svc.Running}
	stream, err := os.OpenFile(h.logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	log, closeLog := logx.Discard, func() {}
	if err == nil {
		log, closeLog = newLogger(h.level, stream, LogFilePath(), os.Stderr)
	}
	opErr := executeOperation(h.op, log)
	closeLog()
	if stream != nil {
		_ = stream.Close()
	}
	_ = writeFileAtomic(h.resultPath, []byte(operationReport(opErr)))
	changes <- svc.Status{State: svc.StopPending}
	_ = winsvc.DeleteOneShot(operationService)
	return false, 0
}

func executeOperation(op string, log *logx.Logger) error {
	switch op {
	case OpInstall:
		return Install(log)
	case OpUninstall:
		return Uninstall(log)
	case OpRestartService:
		return RestartService(log)
	case OpRestartTerm:
		return RestartTermService(log)
	default:
		return fmt.Errorf("unknown operation %q", op)
	}
}
