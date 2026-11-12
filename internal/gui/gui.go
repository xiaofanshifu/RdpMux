package gui

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"

	"rdpmux/internal/app"
	"rdpmux/internal/loc"
	"rdpmux/internal/logx"
	"rdpmux/internal/sysconfig"
	"rdpmux/internal/winutil"
)

type ui struct {
	mw         *walk.MainWindow
	body       *walk.ScrollView
	footer     *walk.Composite
	lsPatch    *walk.Label
	lsService  *walk.Label
	lsListener *walk.Label
	lsTSVer    *walk.Label
	lsSupport  *walk.Label
	lsDB       *walk.Label
	cbAllow    *walk.CheckBox
	cbSingle   *walk.CheckBox
	cbHide     *walk.CheckBox
	cbCustom   *walk.CheckBox
	nePort     *walk.NumberEdit
	nla        []*walk.RadioButton
	shadow     []*walk.RadioButton
	cbLang     *walk.ComboBox
	log        *walk.TextEdit
	bApply     *walk.PushButton
	dirty      bool
	opBusy     bool
	oldPort    uint32
	stopCh     chan struct{}
	lifecycle  sync.Mutex // serializes callback enqueueing with window disposal
	nextState  *uiState

	backend       guiBackend
	enqueue       func(func())
	settings      settingsView
	statusPending atomic.Bool
}

type guiBackend interface {
	Status() app.Status
	WriteSettings(sysconfig.Settings) error
}

type systemBackend struct{}

func (systemBackend) Status() app.Status { return app.GetStatus() }
func (systemBackend) WriteSettings(s sysconfig.Settings) error {
	return sysconfig.WriteSettings(s)
}

// Settings controls are accessed only on the UI thread.
type settingsView interface {
	snapshotSettings() sysconfig.Settings
	setBusy(busy, dirty bool)
	reportSave(error)
	close()
}

type walkSettings struct{ u *ui }

func newUI() *ui {
	u := &ui{stopCh: make(chan struct{}), backend: systemBackend{}}
	u.enqueue = func(f func()) { u.mw.Synchronize(f) }
	u.settings = walkSettings{u}
	return u
}

// uiState holds everything that must survive a language-driven window rebuild.
type uiState struct {
	allow    bool
	single   bool
	hide     bool
	custom   bool
	port     float64
	nla      int
	shadow   int
	oldPort  uint32
	dirty    bool
	logText  string
	bounds   walk.Rectangle
	language string
}

func mf(key, def string) string { return loc.L("MainForm", key, def) }
func st(key, def string) string { return loc.L("Status", key, def) }
func msg(key, def string, args ...any) string {
	return loc.LF("Messages", key, def, args...)
}

func Run() {
	loc.Init("", nil)
	_ = winutil.EnablePrivilege("SeDebugPrivilege")
	var carry *uiState
	for {
		u := newUI()
		if err := u.build(); err != nil {
			walk.MsgBox(nil, "RdpMux", err.Error(), walk.MsgBoxIconError)
			return
		}
		if carry != nil {
			u.restore(*carry)
		} else {
			u.readSettings()
		}
		u.refreshStatusAsync()
		go u.tick()
		u.mw.Run()
		u.stop()
		carry = u.nextState
		if carry == nil {
			return
		}
		if !loc.SetCurrent(carry.language, true) {
			return
		}
	}
}

func (u *ui) build() error {
	nlaItems := strings.Split(mf("rgNLA.Items", "GUI Authentication Only|Default RDP Authentication|Network Level Authentication"), "|")
	shadowItems := strings.Split(mf("rgShadow.Items", "Disable Shadowing|Full access with user's permission|Full access without permission|View only with user's permission|View only without permission"), "|")
	for range nlaItems {
		u.nla = append(u.nla, new(walk.RadioButton))
	}
	for range shadowItems {
		u.shadow = append(u.shadow, new(walk.RadioButton))
	}
	nlaButtons := func() []Widget {
		var w []Widget
		for i, item := range nlaItems {
			w = append(w, RadioButton{AssignTo: &u.nla[i], Text: item, OnClicked: u.markDirty})
		}
		return w
	}()
	shadowButtons := func() []Widget {
		var w []Widget
		for i, item := range shadowItems {
			w = append(w, RadioButton{AssignTo: &u.shadow[i], Text: item, OnClicked: u.markDirty})
		}
		return w
	}()

	langNames := func() []string {
		var s []string
		for _, l := range loc.SupportedLanguages {
			s = append(s, l.NativeName)
		}
		return s
	}()

	err := MainWindow{
		AssignTo: &u.mw,
		Title:    mf("AppTitle", "RdpMux - RDP Configuration"),
		Size:     Size{Width: 720, Height: 720},
		MinSize:  Size{Width: 620, Height: 480},
		Layout:   VBox{Margins: Margins{Left: 8, Top: 8, Right: 8, Bottom: 8}, Spacing: 6},
		Children: []Widget{
			ScrollView{
				AssignTo: &u.body,
				Layout:   VBox{Spacing: 6},
				Children: []Widget{
					GroupBox{
						Title:  mf("gbDiag.Caption", "Diagnostics"),
						Layout: VBox{Spacing: 4},
						Children: []Widget{
							u.diagRow(mf("lPatch.Caption", "Patch state:"), &u.lsPatch),
							u.diagRow(mf("lService.Caption", "Service state:"), &u.lsService),
							u.diagRow(mf("lListener.Caption", "Listener state:"), &u.lsListener),
							Composite{Layout: HBox{Spacing: 6}, Children: []Widget{
								Label{Text: mf("lTSVer.Caption", "termsrv ver."), MinSize: Size{Width: 110}},
								Label{AssignTo: &u.lsTSVer, Text: "-"},
								Label{AssignTo: &u.lsSupport, Text: ""},
							}},
							u.diagRow(mf("lDB.Caption", "Patch DB:"), &u.lsDB),
						},
					},
					GroupBox{
						Title:  mf("gbGeneral.Caption", "General Settings"),
						Layout: VBox{Spacing: 4},
						Children: []Widget{
							CheckBox{AssignTo: &u.cbAllow, Text: mf("cbAllowTSConnections.Caption", "Enable Remote Desktop"), OnCheckedChanged: u.markDirty},
							Composite{Layout: HBox{Spacing: 6}, Children: []Widget{
								Label{Text: mf("lRDPPort.Caption", "RDP port:"), MinSize: Size{Width: 110}},
								NumberEdit{AssignTo: &u.nePort, MinValue: 1, MaxValue: 65535, Decimals: 0, OnValueChanged: u.markDirty, MaxSize: Size{Width: 100}},
							}},
							CheckBox{AssignTo: &u.cbSingle, Text: mf("cbSingleSessionPerUser.Caption", "Single session per user"), OnCheckedChanged: u.markDirty},
							CheckBox{AssignTo: &u.cbHide, Text: mf("cbHideUsers.Caption", "Hide users on logon screen"), OnCheckedChanged: u.markDirty},
							CheckBox{AssignTo: &u.cbCustom, Text: mf("cbCustomPrg.Caption", "Allow unlisted RemoteApps"), OnCheckedChanged: u.markDirty},
							Composite{Layout: HBox{Spacing: 6}, Children: []Widget{
								GroupBox{Title: mf("rgNLA.Caption", "Authentication Mode"), Layout: VBox{Spacing: 2}, Children: nlaButtons},
								GroupBox{Title: mf("rgShadow.Caption", "Session Shadowing Mode"), Layout: VBox{Spacing: 2}, Children: shadowButtons},
							}},
						},
					},
					GroupBox{
						Title:  mf("gbLocalRDPChecker.Caption", "Check your settings with mstsc"),
						Layout: HBox{Spacing: 6},
						Children: []Widget{
							PushButton{Text: mf("bmstsc.Caption", "Fullscreen"), OnClicked: func() { u.mstsc("/f") }},
							PushButton{Text: mf("b800x600.Caption", "800x600"), OnClicked: func() { u.mstsc("/w:800 /h:600") }},
							PushButton{Text: mf("b1024x768.Caption", "1024x768"), OnClicked: func() { u.mstsc("/w:1024 /h:768") }},
							PushButton{Text: mf("b1366x768.Caption", "1366x768"), OnClicked: func() { u.mstsc("/w:1366 /h:768") }},
							PushButton{Text: mf("b1920x1080.Caption", "1920x1080"), OnClicked: func() { u.mstsc("/w:1920 /h:1080") }},
						},
					},
					Composite{Layout: HBox{Spacing: 6}, Children: []Widget{
						PushButton{Text: mf("bInstall.Caption", "Install"), OnClicked: func() {
							u.runOp(msg("OpInstall", "install"), func(log *logx.Logger) error { return app.Install(log) })
						}},
						PushButton{Text: mf("bUninstall.Caption", "Uninstall"), OnClicked: u.onUninstall},
						PushButton{Text: mf("bUpdateINI.Caption", "Update INI"), OnClicked: func() {
							u.runOp(msg("OpUpdate", "update"), func(log *logx.Logger) error { return app.Update("", log) })
						}},
						PushButton{Text: mf("bRestartTS.Caption", "Restart TermService"), OnClicked: func() {
							u.runOp(msg("OpRestart", "restart"), func(log *logx.Logger) error {
								log.Info("%s", msg("LogRestartingTermService", "restarting TermService..."))
								return sysconfig.RestartTermService()
							})
						}},
					}},
					TextEdit{AssignTo: &u.log, ReadOnly: true, VScroll: true, MinSize: Size{Height: 90}},
				},
			},
			Composite{AssignTo: &u.footer, Layout: HBox{Spacing: 6}, Children: []Widget{
				Label{Text: mf("lLanguage.Caption", "Language:")},
				ComboBox{AssignTo: &u.cbLang, Model: langNames, MaxSize: Size{Width: 140}, OnCurrentIndexChanged: u.onLanguage},
				HSpacer{},
				PushButton{AssignTo: &u.bApply, Text: mf("bApply.Caption", "Apply"), Enabled: false, OnClicked: u.onApply},
				PushButton{Text: mf("bOK.Caption", "OK"), OnClicked: u.onOK},
				PushButton{Text: mf("bCancel.Caption", "Cancel"), OnClicked: u.onCancel},
			}},
		},
	}.Create()
	if err != nil {
		return err
	}
	u.mw.Disposing().Attach(u.stop)
	u.mw.Closing().Attach(u.onClosing)
	u.setLangCombo()
	if font := loc.CurrentLanguageFont(); font != "" {
		if f, err := walk.NewFont(font, 9, 0); err == nil {
			u.mw.SetFont(f)
		}
	}
	return nil
}

func (u *ui) onClosing(canceled *bool, reason walk.CloseReason) {
	if u.opBusy {
		*canceled = true
		return
	}
	if u.nextState == nil && u.dirty {
		answer := walk.MsgBox(u.mw, msg("Warning", "Warning"),
			msg("UnsavedChanges", "Settings are not saved. Do you want to exit?"),
			walk.MsgBoxYesNo|walk.MsgBoxIconWarning)
		if answer != win.IDYES {
			*canceled = true
			return
		}
	}
	u.stop()
}

func (u *ui) snapshot(language string) uiState {
	return uiState{
		allow:    u.cbAllow.Checked(),
		single:   u.cbSingle.Checked(),
		hide:     u.cbHide.Checked(),
		custom:   u.cbCustom.Checked(),
		port:     u.nePort.Value(),
		nla:      selectedIndex(u.nla),
		shadow:   selectedIndex(u.shadow),
		oldPort:  u.oldPort,
		dirty:    u.dirty,
		logText:  u.log.Text(),
		bounds:   u.mw.Bounds(),
		language: language,
	}
}

func (u *ui) restore(s uiState) {
	u.cbAllow.SetChecked(s.allow)
	u.cbSingle.SetChecked(s.single)
	u.cbHide.SetChecked(s.hide)
	u.cbCustom.SetChecked(s.custom)
	u.nePort.SetValue(s.port)
	if s.nla >= 0 && s.nla < len(u.nla) {
		u.nla[s.nla].SetChecked(true)
	}
	if s.shadow >= 0 && s.shadow < len(u.shadow) {
		u.shadow[s.shadow].SetChecked(true)
	}
	u.oldPort = s.oldPort
	u.dirty = s.dirty
	u.bApply.SetEnabled(s.dirty)
	if s.logText != "" {
		u.log.SetText(s.logText)
	}
	if !s.bounds.IsZero() {
		u.mw.SetBounds(s.bounds)
	}
}

func (u *ui) diagRow(caption string, target **walk.Label) Widget {
	return Composite{Layout: HBox{Spacing: 6}, Children: []Widget{
		Label{Text: caption, MinSize: Size{Width: 110}},
		Label{AssignTo: target, Text: "-"},
	}}
}

func (u *ui) markDirty() {
	u.dirty = true
	if u.bApply != nil {
		u.bApply.SetEnabled(!u.opBusy)
	}
}

// Busy state and all widget access are confined to the UI thread.
func (u *ui) setBusy(busy bool) {
	u.opBusy = busy
	u.settings.setBusy(busy, u.dirty)
}

func (v walkSettings) setBusy(busy, dirty bool) {
	v.u.body.SetEnabled(!busy)
	v.u.footer.SetEnabled(!busy)
	v.u.bApply.SetEnabled(!busy && dirty)
}

func (u *ui) readSettings() {
	s := sysconfig.ReadSettings()
	u.cbAllow.SetChecked(s.AllowTSConnections)
	u.cbSingle.SetChecked(s.SingleSessionPerUser)
	u.cbHide.SetChecked(s.HideUsers)
	u.cbCustom.SetChecked(s.HonorLegacySettings)
	u.nePort.SetValue(float64(s.Port))
	u.oldPort = s.Port
	if s.NLA >= 0 && s.NLA < len(u.nla) {
		u.nla[s.NLA].SetChecked(true)
	}
	if s.Shadow >= 0 && s.Shadow < len(u.shadow) {
		u.shadow[s.Shadow].SetChecked(true)
	}
	u.dirty = false
	u.bApply.SetEnabled(false)
}

func selectedIndex(buttons []*walk.RadioButton) int {
	for i, b := range buttons {
		if b.Checked() {
			return i
		}
	}
	return -1
}

func (v walkSettings) snapshotSettings() sysconfig.Settings {
	u := v.u
	return sysconfig.Settings{
		AllowTSConnections:   u.cbAllow.Checked(),
		SingleSessionPerUser: u.cbSingle.Checked(),
		HonorLegacySettings:  u.cbCustom.Checked(),
		Port:                 uint32(u.nePort.Value()),
		NLA:                  selectedIndex(u.nla),
		Shadow:               selectedIndex(u.shadow),
		HideUsers:            u.cbHide.Checked(),
	}
}

func (v walkSettings) reportSave(err error) {
	if err != nil {
		v.u.logLine(msg("LogSettingsError", "settings error: %s", err))
		walk.MsgBox(v.u.mw, msg("Error", "Error"), err.Error(), walk.MsgBoxIconError)
		return
	}
	v.u.logLine(msg("LogSettingsSaved", "settings saved"))
}

func (v walkSettings) close() { v.u.mw.Close() }

// The returned channel tracks the worker, not execution of the UI callback.
func (u *ui) writeSettings(closeOnSuccess bool) <-chan struct{} {
	if u.opBusy || u.stopped() {
		return nil
	}
	s := u.settings.snapshotSettings()
	u.setBusy(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := u.backend.WriteSettings(s)
		u.post(func() {
			if err == nil {
				u.oldPort = s.Port
				u.dirty = false
			}
			u.settings.reportSave(err)
			u.setBusy(false)
			if err == nil && closeOnSuccess {
				u.settings.close()
			}
		})
	}()
	return done
}

func (u *ui) onApply() { u.writeSettings(false) }

func (u *ui) onOK() {
	if u.opBusy || u.stopped() {
		return
	}
	if u.dirty {
		u.writeSettings(true)
		return
	}
	u.settings.close()
}

func (u *ui) onCancel() {
	if !u.opBusy && !u.stopped() {
		u.settings.close()
	}
}

func (u *ui) onUninstall() {
	if u.opBusy || u.stopped() {
		return
	}
	msgText := msg("UninstallConfirm", "Uninstall RdpMux and restore stock single-session behavior? Active RDP sessions will disconnect.")
	if walk.MsgBox(u.mw, msg("Warning", "Warning"), msgText, walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != win.IDYES {
		return
	}
	u.runOp(msg("OpUninstall", "uninstall"), func(log *logx.Logger) error { return app.Uninstall(false, log) })
}

func (u *ui) stopped() bool {
	select {
	case <-u.stopCh:
		return true
	default:
		return false
	}
}

func (u *ui) stop() {
	u.lifecycle.Lock()
	defer u.lifecycle.Unlock()
	if !u.stopped() {
		close(u.stopCh)
	}
}

// Queue callbacks before handle disposal, and discard them after closing.
func (u *ui) post(f func()) {
	u.lifecycle.Lock()
	defer u.lifecycle.Unlock()
	if u.stopped() {
		return
	}
	u.enqueue(func() {
		if !u.stopped() {
			f()
		}
	})
}

type guiWriter struct{ u *ui }

func (w guiWriter) Write(p []byte) (int, error) {
	s := string(p)
	w.u.post(func() { w.u.log.AppendText(s) })
	return len(p), nil
}

func (u *ui) runOp(label string, op func(*logx.Logger) error) {
	if u.opBusy || u.stopped() {
		return
	}
	u.setBusy(true)
	u.logLine("=== " + label + " ===")
	go func() {
		// Finish cleanup before the UI can enable another operation or close.
		err, logErr := func() (error, error) {
			log, closeLog := app.NewLogger(logx.ResolveLevel("", app.ExeDir()), guiWriter{u})
			defer closeLog()
			err := op(log)
			return err, log.Err()
		}()
		u.post(func() {
			if err != nil {
				u.logLine(msg("LogOpError", "error: %s", err))
			}
			if logErr != nil {
				u.logLine(msg("LogOpError", "error: %s", logErr))
			}
			if err == nil && !u.dirty {
				u.readSettings()
			}
			u.refreshStatusAsync()
			u.setBusy(false)
		})
	}()
}

func (u *ui) logLine(s string) {
	u.log.AppendText(time.Now().Format("15:04:05 ") + s + "\r\n")
}

func mstscArgs(savedPort uint32, args string) []string {
	return strings.Fields(fmt.Sprintf("/v:127.0.0.2:%d %s /prompt", savedPort, args))
}

func (u *ui) mstsc(args string) {
	if u.opBusy || u.stopped() {
		return
	}
	cmd := exec.Command("mstsc", mstscArgs(u.oldPort, args)...)
	if err := cmd.Start(); err != nil {
		u.logLine(msg("LogOpError", "error: %s", err))
		walk.MsgBox(u.mw, msg("Error", "Error"), err.Error(), walk.MsgBoxIconError)
		return
	}
	go cmd.Wait() // Reap the child without blocking the UI.
}

func (u *ui) onLanguage() {
	if u.opBusy || u.stopped() {
		return
	}
	idx := u.cbLang.CurrentIndex()
	if idx < 0 || idx >= len(loc.SupportedLanguages) {
		return
	}
	code := loc.SupportedLanguages[idx].Code
	if code == loc.CurrentLanguage() {
		return
	}
	state := u.snapshot(code)
	u.nextState = &state
	u.mw.Close()
}

func (u *ui) setLangCombo() {
	for i, l := range loc.SupportedLanguages {
		if l.Code == loc.CurrentLanguage() {
			u.cbLang.SetCurrentIndex(i)
			return
		}
	}
}

func termStateText(state uint32) string {
	switch state {
	case 1:
		return st("Stopped", "Stopped")
	case 2:
		return st("Starting", "Starting...")
	case 3:
		return st("Stopping", "Stopping...")
	case 4:
		return st("Running", "Running")
	case 5:
		return st("Resuming", "Resuming...")
	case 6:
		return st("Suspending", "Suspending...")
	case 7:
		return st("Suspended", "Suspended")
	default:
		return st("Unknown", "Unknown")
	}
}

// Serialize queries across window rebuilds without dropping the new window's query.
var statusQueryGate = make(chan struct{}, 1)

func (u *ui) refreshStatusAsync() <-chan struct{} {
	if u.stopped() || !u.statusPending.CompareAndSwap(false, true) {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer u.statusPending.Store(false)
		select {
		case statusQueryGate <- struct{}{}:
		case <-u.stopCh:
			return
		}
		s, ok := func() (app.Status, bool) {
			defer func() { <-statusQueryGate }()
			if u.stopped() {
				return app.Status{}, false
			}
			return u.backend.Status(), true
		}()
		if ok {
			u.post(func() { u.applyStatus(s) })
		}
	}()
	return done
}

func (u *ui) applyStatus(s app.Status) {
	if s.TermsrvErr != nil {
		u.lsTSVer.SetText(st("NA", "N/A"))
		u.lsSupport.SetText("")
	} else {
		u.lsTSVer.SetText(s.TermsrvVersion)
		if s.Supported {
			u.lsSupport.SetText(st("FullySupported", "[fully supported]"))
		} else {
			u.lsSupport.SetText(st("NotSupported", "[not supported]"))
		}
	}
	if s.DBErr != nil {
		u.lsDB.SetText(s.DBErr.Error())
	} else {
		u.lsDB.SetText(s.DBUpdated)
	}
	if s.TermErr != nil {
		u.lsService.SetText(st("Unknown", "Unknown"))
	} else {
		u.lsService.SetText(fmt.Sprintf("%s (pid %d)", termStateText(s.TermState), s.TermPID))
	}
	switch {
	case s.TermErr != nil || s.TermState != 4:
		u.lsPatch.SetText(st("NA", "N/A"))
	case s.PatchNeedAdmin:
		u.lsPatch.SetText(st("NeedAdmin", "run as administrator"))
	case s.PatchErr != nil:
		u.lsPatch.SetText(s.PatchErr.Error())
	case s.PatchActive:
		u.lsPatch.SetText(st("Patched", "ACTIVE"))
	default:
		u.lsPatch.SetText(st("NotPatched", "inactive"))
	}
	if s.Listener {
		u.lsListener.SetText(st("Listening", "Listening"))
	} else {
		u.lsListener.SetText(st("NotListening", "Not listening"))
	}
}

func (u *ui) tick() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-u.stopCh:
			return
		case <-t.C:
			u.refreshStatusAsync()
		}
	}
}
