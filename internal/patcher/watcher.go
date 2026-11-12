package patcher

import (
	"context"
	"fmt"
	"time"

	"rdpmux/internal/logx"
	"rdpmux/internal/winutil"
)

type Watcher struct {
	DB   *DB
	Arch string
	Log  *logx.Logger
}

func (w *Watcher) logger() *logx.Logger {
	if w.Log != nil {
		return w.Log
	}
	return logx.Discard
}

func (w *Watcher) attach(pid uint32) (*Patcher, error) {
	return New(pid, w.DB, w.Arch, w.logger())
}

func (w *Watcher) RunOnce(ctx context.Context, timeout time.Duration) error {
	log := w.logger()
	deadline := time.Now().Add(timeout)
	var p *Patcher
	defer func() {
		if p != nil {
			p.Close()
		}
	}()
	for {
		if p == nil {
			state, pid, err := winutil.SvcQueryState("TermService")
			if err != nil {
				return err
			}
			if state != 4 || pid == 0 {
				return fmt.Errorf("TermService is not running")
			}
			p, err = w.attach(pid)
			if err != nil {
				return err
			}
			log.Info("%s", tr("Attached", "attached: termsrv.dll %s (pid %d)", p.Version, pid))
		}
		if _, err := p.Apply(); err != nil {
			return err
		}
		if ok, err := p.Verify(); err != nil {
			return err
		} else if ok {
			log.Info("%s", tr("Verified", "verified: all patches active"))
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for CSLQuery::Initialize (bInitialized)")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (w *Watcher) Run(ctx context.Context) {
	log := w.logger()
	var p *Patcher
	var lastPID uint32
	var lastErr string
	defer func() {
		if p != nil {
			p.Close()
		}
	}()
	log.Info("%s", tr("Started", "watcher started (arch=%s, ini updated=%s)", w.Arch, w.DB.Updated))
	for {
		interval := 5 * time.Second
		state, pid, err := winutil.SvcQueryState("TermService")
		switch {
		case err != nil || state != 4 || pid == 0:
			if p != nil {
				p.Close()
				p = nil
				log.Info("%s", tr("Detached", "TermService stopped, detached"))
			}
		default:
			if p != nil && pid != lastPID {
				p.Close()
				p = nil
				log.Info("%s", tr("PIDChanged", "TermService PID changed %d -> %d, reattaching", lastPID, pid))
			}
			if p == nil {
				p, err = w.attach(pid)
				if err != nil {
					if err.Error() != lastErr {
						log.Warn("%s", tr("AttachErr", "attach: %s", err))
						lastErr = err.Error()
					}
					p = nil
					break
				}
				lastPID = pid
				lastErr = ""
				log.Info("%s", tr("AttachedBase", "attached: termsrv.dll %s (pid %d, base=0x%X)", p.Version, pid, p.base))
			}
			ok, err := p.Verify()
			if err != nil {
				log.Warn("%s", tr("VerifyErr", "verify: %s", err))
				p.Close()
				p = nil
				break
			}
			if !ok {
				interval = 500 * time.Millisecond
				if _, err := p.Apply(); err != nil {
					log.Error("%s", tr("ApplyCodeErr", "apply patches: %s", err))
					p.Close()
					p = nil
				}
			}
		}
		select {
		case <-ctx.Done():
			log.Info("%s", tr("Stopping", "watcher stopping"))
			return
		case <-time.After(interval):
		}
	}
}
