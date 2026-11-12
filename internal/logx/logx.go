package logx

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

type Level int

const (
	Error Level = iota
	Warn
	Info
	Debug
	Trace
)

func (l Level) String() string {
	switch l {
	case Error:
		return "ERROR"
	case Warn:
		return "WARN"
	case Info:
		return "INFO"
	case Debug:
		return "DEBUG"
	case Trace:
		return "TRACE"
	default:
		return "?"
	}
}

func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error", "0":
		return Error, true
	case "warn", "warning", "1":
		return Warn, true
	case "info", "2":
		return Info, true
	case "debug", "3":
		return Debug, true
	case "trace", "4":
		return Trace, true
	default:
		return Info, false
	}
}

type Logger struct {
	mu      sync.Mutex
	level   Level
	writers []io.Writer
	err     error
}

var Discard = &Logger{level: Error}

func New(level Level, writers ...io.Writer) *Logger {
	return &Logger{level: level, writers: writers}
}

func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	l.level = level
	l.mu.Unlock()
}

func (l *Logger) Level() Level {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

func (l *Logger) log(level Level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if level > l.level || len(l.writers) == 0 {
		return
	}
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s [%-5s] %s\r\n", time.Now().Format("2006-01-02 15:04:05.000"), level.String(), msg)
	for _, w := range l.writers {
		n, err := io.WriteString(w, line)
		if err == nil && n != len(line) {
			err = io.ErrShortWrite
		}
		if err != nil && l.err == nil {
			l.err = err
		}
	}
}

// Err reports the first writer error, if any.
func (l *Logger) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *Logger) Error(format string, args ...any) { l.log(Error, format, args...) }
func (l *Logger) Warn(format string, args ...any)  { l.log(Warn, format, args...) }
func (l *Logger) Info(format string, args ...any)  { l.log(Info, format, args...) }
func (l *Logger) Debug(format string, args ...any) { l.log(Debug, format, args...) }
func (l *Logger) Trace(format string, args ...any) { l.log(Trace, format, args...) }

type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	mutex    windows.Handle
}

const rotationLockTimeout = 5000 // milliseconds

func NewRotatingFile(path string, maxBytes int64) (*RotatingFile, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	// Resolve directory aliases even before the log itself exists.
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(dir, filepath.Base(path))
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	hash := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(path))))
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`Global\RdpMux.Log.%x`, hash))
	if err != nil {
		return nil, err
	}
	// The service and GUI run in different Windows sessions.
	handle, err := windows.CreateMutexEx(nil, name, 0, windows.SYNCHRONIZE|windows.MUTEX_MODIFY_STATE)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, fmt.Errorf("create log mutex: %w", err)
	}
	w := &RotatingFile{path: path, maxBytes: maxBytes, mutex: handle}
	// Validate/create the destination, but never keep its file handle open.
	if _, err := w.Write(nil); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

func (w *RotatingFile) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.mutex == 0 {
		return 0, os.ErrClosed
	}
	// Windows mutex ownership belongs to an OS thread, not a goroutine.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	result, err := windows.WaitForSingleObject(w.mutex, rotationLockTimeout)
	if err != nil {
		return 0, fmt.Errorf("wait for log mutex: %w", err)
	}
	switch result {
	case windows.WAIT_OBJECT_0, windows.WAIT_ABANDONED:
		// An abandoned mutex is acquired too. No cached file state is used.
	case uint32(windows.WAIT_TIMEOUT):
		return 0, fmt.Errorf("wait for log mutex: %w", windows.WAIT_TIMEOUT)
	default:
		return 0, fmt.Errorf("unexpected log mutex wait result: %#x", result)
	}
	defer func() {
		if releaseErr := windows.ReleaseMutex(w.mutex); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release log mutex: %w", releaseErr))
		}
	}()

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return 0, err
	}
	defer func() {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if len(p) > 0 && fi.Size() > 0 && fi.Size()+int64(len(p)) > w.maxBytes {
		closeErr := f.Close()
		f = nil
		if closeErr != nil {
			return 0, closeErr
		}
		if err := os.Rename(w.path, w.path+".1"); err != nil {
			return 0, err
		}
		f, err = os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return 0, err
		}
	}
	return f.Write(p)
}

func (w *RotatingFile) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.mutex != 0 {
		_ = windows.CloseHandle(w.mutex)
		w.mutex = 0
	}
}

func LevelFromConfig(dir string) (Level, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "rdpmux.conf"))
	if err != nil {
		return Info, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "log-level") {
			if lv, ok := ParseLevel(value); ok {
				return lv, true
			}
		}
	}
	return Info, false
}

func ResolveLevel(flag, dir string) Level {
	if flag != "" {
		if lv, ok := ParseLevel(flag); ok {
			return lv
		}
	}
	if lv, ok := ParseLevel(os.Getenv("RDPMUX_LOG_LEVEL")); ok {
		return lv
	}
	if lv, ok := LevelFromConfig(dir); ok {
		return lv
	}
	return Info
}
