package gui

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows/svc"

	"rdpmux/internal/app"
	"rdpmux/internal/loc"
	"rdpmux/internal/logx"
	"rdpmux/internal/sysconfig"
	"rdpmux/internal/winutil"
)

type ui struct {
	mw          *walk.MainWindow
	body        *walk.Composite
	footer      *walk.Composite
	lsPatch     *walk.Label
	lsService   *walk.Label
	lsSvc       *walk.Label
	lsListener  *walk.Label
	lsTSVer     *walk.Label
	lsSupport   *walk.Label
	lsDB        *walk.Label
	dotService  *colorDot
	dotSvc      *colorDot
	dotPatch    *colorDot
	dotListener *colorDot
	dotVersion  *colorDot
	cbAllow     *walk.CheckBox
	cbSingle    *walk.CheckBox
	cbHide      *walk.CheckBox
	cbCustom    *walk.CheckBox
	nePort      *walk.NumberEdit
	nla         []*walk.RadioButton
	shadow      []*walk.RadioButton
	cbLang      *walk.ComboBox
	log         *walk.TextEdit
	bApply      *walk.PushButton
	dirty       bool
	opBusy      bool
	oldPort     uint32
	stopCh      chan struct{}
	lifecycle   sync.Mutex // serializes callback enqueueing with window disposal
	nextState   *uiState

	backend       guiBackend
	enqueue       func(func())
	settings      settingsView
	statusPending atomic.Bool
}

type guiBackend interface {
	Status() app.Status
	WriteSettings(sysconfig.Settings) error
	RunOperation(op string, display io.Writer) error
}

type systemBackend struct{}

func (systemBackend) Status() app.Status { return app.GetStatus() }
func (systemBackend) WriteSettings(s sysconfig.Settings) error {
	return sysconfig.WriteSettings(s)
}
func (systemBackend) RunOperation(op string, display io.Writer) error {
	return app.RunOperation(op, logx.ResolveLevel("", app.ExeDir()), display)
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
	u := &ui{
		stopCh:      make(chan struct{}),
		backend:     systemBackend{},
		dotService:  newColorDot(),
		dotSvc:      newColorDot(),
		dotPatch:    newColorDot(),
		dotListener: newColorDot(),
		dotVersion:  newColorDot(),
	}
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

const spiGetWorkArea = 0x0030

var (
	colorOK    = walk.RGB(0x1b, 0x7f, 0x3b)
	colorBad   = walk.RGB(0xc0, 0x35, 0x2b)
	colorIdle  = walk.RGB(0x80, 0x80, 0x80)
	colorMuted = walk.RGB(0x60, 0x60, 0x60)
)

// Status dots are custom-painted squares. Labels cannot be relied on here:
// their text color only takes effect through WM_CTLCOLORSTATIC.
var dotBrushes = map[walk.Color]walk.Brush{}

func setDot(dot *colorDot, color walk.Color) { dot.set(color) }

type colorDot struct {
	widget *walk.CustomWidget
	color  walk.Color
}

func newColorDot() *colorDot { return &colorDot{color: colorIdle} }

func (d *colorDot) paint(canvas *walk.Canvas, _ walk.Rectangle) error {
	brush, ok := dotBrushes[d.color]
	if !ok {
		var err error
		if brush, err = walk.NewSolidColorBrush(d.color); err != nil {
			return err
		}
		dotBrushes[d.color] = brush
	}
	// The layout stretches the widget vertically, so draw a centered square
	// instead of a tall bar.
	bounds := d.widget.ClientBounds()
	size := min(bounds.Width, bounds.Height)
	bounds.X += (bounds.Width - size) / 2
	bounds.Y += (bounds.Height - size) / 2
	bounds.Width, bounds.Height = size, size
	return canvas.FillRectangle(brush, bounds)
}

func (d *colorDot) set(color walk.Color) {
	if d == nil || d.color == color {
		return
	}
	d.color = color
	if d.widget != nil {
		d.widget.Invalidate()
	}
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
			u.centerOnScreen()
			// The first ShowWindow still applies the startup placement, so
			// center again as soon as the message loop is running.
			u.mw.Starting().Attach(func() { u.mw.Synchronize(u.centerOnScreen) })
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

	// The language font must be set before the controls are created: applying it
	// afterwards walks the whole tree and relayouts once per widget (~10s).
	windowFont := Font{}
	if family := loc.CurrentLanguageFont(); family != "" {
		windowFont = Font{Family: family, PointSize: 9}
	}

	err := MainWindow{
		AssignTo: &u.mw,
		Title:    mf("AppTitle", "RdpMux - RDP Configuration"),
		Font:     windowFont,
		Size:     Size{Width: 850, Height: 680},
		MinSize:  Size{Width: 800, Height: 640},
		Layout: VBox{
			Margins:   Margins{Left: 10, Top: 10, Right: 10, Bottom: 10},
			Spacing:   8,
			Alignment: AlignHNearVNear,
		},
		Children: []Widget{
			u.statusStrip(),
			Composite{
				AssignTo: &u.body,
				Layout:   HBox{Spacing: 10, MarginsZero: true, Alignment: AlignHNearVNear},
				MaxSize:  Size{Width: 32767, Height: 32767},
				Children: []Widget{
					u.actionColumn(),
					u.settingsGroup(nlaButtons, shadowButtons),
				},
			},
			u.logPanel(),
			Composite{AssignTo: &u.footer, Layout: HBox{Spacing: 6, MarginsZero: true, Alignment: AlignHNearVNear}, Children: []Widget{
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
	return nil
}

// The strip answers "can I connect right now" without reading the log.
func (u *ui) statusStrip() Widget {
	return Composite{
		Layout:  HBox{Spacing: 28, MarginsZero: true, Alignment: AlignHNearVNear},
		MaxSize: Size{Width: 32767},
		Children: []Widget{
			statusItem(mf("sbService.Caption", "TermService"), u.dotService, &u.lsService),
			statusItem(mf("sbPatchService.Caption", "Patch service"), u.dotSvc, &u.lsSvc),
			statusItem(mf("sbPatch.Caption", "Patch"), u.dotPatch, &u.lsPatch),
			statusItem(mf("sbListener.Caption", "RDP listener"), u.dotListener, &u.lsListener),
			statusItem(mf("lTSVer.Caption", "termsrv ver."), u.dotVersion, &u.lsTSVer, &u.lsSupport),
		},
	}
}

func statusItem(caption string, dot *colorDot, values ...**walk.Label) Widget {
	row := []Widget{CustomWidget{
		AssignTo: &dot.widget,
		MinSize:  Size{Width: 10, Height: 10},
		MaxSize:  Size{Width: 10, Height: 10},
		Paint:    dot.paint,
	}}
	for i, v := range values {
		text := ""
		if i == 0 {
			text = "-"
		}
		row = append(row, Label{AssignTo: v, Text: text})
	}
	return Composite{
		Layout: VBox{Spacing: 2, MarginsZero: true, Alignment: AlignHNearVNear},
		Children: []Widget{
			Label{Text: caption, TextColor: colorMuted},
			Composite{Layout: HBox{Spacing: 6, MarginsZero: true, Alignment: AlignHNearVCenter}, Children: row},
		},
	}
}

// Operations live in a fixed-width column so the settings keep a stable place.
func (u *ui) actionColumn() Widget {
	button := func(text, tooltip string, onClick func()) PushButton {
		return PushButton{Text: text, ToolTipText: tooltip, MaxSize: Size{Width: 32767}, OnClicked: onClick}
	}
	row := func(children ...Widget) Widget {
		return Composite{Layout: HBox{Spacing: 6, MarginsZero: true, Alignment: AlignHNearVNear}, Children: children}
	}
	return Composite{
		Layout:  VBox{Spacing: 8, MarginsZero: true, Alignment: AlignHNearVNear},
		MinSize: Size{Width: 320},
		MaxSize: Size{Width: 320},
		Children: []Widget{
			GroupBox{
				Title:   mf("gbActions.Caption", "Actions"),
				Layout:  VBox{Spacing: 10, Alignment: AlignHNearVNear},
				MaxSize: Size{Width: 32767},
				Children: []Widget{
					row(Label{Text: mf("lDB.Caption", "Patch DB:")}, Label{AssignTo: &u.lsDB, Text: "-"}),
					row(
						button(mf("bInstall.Caption", "Install"), "", func() {
							u.runOperation(msg("OpInstall", "install"), app.OpInstall)
						}),
						button(mf("bUpdateINI.Caption", "Update INI"), mf("bUpdateINI.ToolTip", "Only downloads the latest patch database from GitHub; it does not apply it. Use Restart Service to apply."), func() {
							u.runOp(msg("OpUpdate", "update"), func(log *logx.Logger) error { return app.Update("", log) })
						}),
					),
					row(
						button(mf("bRestartService.Caption", "Restart Service"), mf("bRestartService.ToolTip", "Restarts the RdpMux service to load and apply the current patch database. If the patches for the running termsrv version changed, TermService is also restarted (active RDP sessions disconnect)."), func() {
							u.runOperation(msg("OpRestartService", "restart service"), app.OpRestartService)
						}),
						button(mf("bRestartTS.Caption", "Restart TermService"), mf("bRestartTS.ToolTip", "Restarts TermService only (disconnects active RDP sessions). Does not reload the patch database; use Restart Service to apply an updated database."), func() {
							u.runOperation(msg("OpRestart", "restart"), app.OpRestartTerm)
						}),
					),
					row(button(mf("bUninstall.Caption", "Uninstall"), "", u.onUninstall), HSpacer{}),
					VSpacer{},
				},
			},
			GroupBox{
				Title:   mf("gbTest.Caption", "Test connection"),
				Layout:  VBox{Spacing: 10, Alignment: AlignHNearVNear},
				MaxSize: Size{Width: 32767},
				Children: []Widget{
					row(button(mf("bmstsc.Caption", "Fullscreen"), "", func() { u.mstsc("/f") })),
					row(
						button(mf("b800x600.Caption", "800x600"), "", func() { u.mstsc("/w:800 /h:600") }),
						button(mf("b1024x768.Caption", "1024x768"), "", func() { u.mstsc("/w:1024 /h:768") }),
					),
					row(
						button(mf("b1366x768.Caption", "1366x768"), "", func() { u.mstsc("/w:1366 /h:768") }),
						button(mf("b1920x1080.Caption", "1920x1080"), "", func() { u.mstsc("/w:1920 /h:1080") }),
					),
					VSpacer{},
				},
			},
		},
	}
}

func (u *ui) settingsGroup(nla, shadow []Widget) Widget {
	return GroupBox{
		Title:  mf("gbGeneral.Caption", "General Settings"),
		Layout: VBox{Spacing: 6, Alignment: AlignHNearVNear},
		Children: []Widget{
			CheckBox{AssignTo: &u.cbAllow, Text: mf("cbAllowTSConnections.Caption", "Enable Remote Desktop"), OnCheckedChanged: u.markDirty},
			Composite{Layout: HBox{Spacing: 8, MarginsZero: true, Alignment: AlignHNearVNear}, Children: []Widget{
				Label{Text: mf("lRDPPort.Caption", "RDP port:"), MinSize: Size{Width: 76}},
				NumberEdit{AssignTo: &u.nePort, MinValue: 1, MaxValue: 65535, Decimals: 0, OnValueChanged: u.markDirty, MinSize: Size{Width: 90}, MaxSize: Size{Width: 100}, ToolTipText: mf("nePort.ToolTip", "Make sure Windows Firewall allows this port; RdpMux does not manage firewall rules.")},
				HSpacer{},
			}},
			CheckBox{AssignTo: &u.cbSingle, Text: mf("cbSingleSessionPerUser.Caption", "Single session per user"), OnCheckedChanged: u.markDirty},
			CheckBox{AssignTo: &u.cbHide, Text: mf("cbHideUsers.Caption", "Hide users on logon screen"), OnCheckedChanged: u.markDirty},
			CheckBox{AssignTo: &u.cbCustom, Text: mf("cbCustomPrg.Caption", "Allow unlisted RemoteApps"), OnCheckedChanged: u.markDirty},
			Composite{Layout: HBox{Spacing: 8, MarginsZero: true, Alignment: AlignHNearVNear}, Children: []Widget{
				GroupBox{Title: mf("rgNLA.Caption", "Authentication Mode"), Layout: VBox{Spacing: 4, Alignment: AlignHNearVNear}, Children: nla},
				GroupBox{Title: mf("rgShadow.Caption", "Session Shadowing Mode"), Layout: VBox{Spacing: 4, Alignment: AlignHNearVNear}, Children: shadow},
				HSpacer{},
			}},
			// Spacers make this group report growable, so it fills the free space.
			HSpacer{},
			VSpacer{},
		},
	}
}

// The log is fixed to the window, so operation output never scrolls away.
func (u *ui) logPanel() Widget {
	return GroupBox{
		Title:   mf("gbLog.Caption", "Log"),
		Layout:  VBox{Spacing: 4, Alignment: AlignHNearVNear},
		MinSize: Size{Height: 120},
		MaxSize: Size{Width: 32767, Height: 240},
		Children: []Widget{
			TextEdit{AssignTo: &u.log, ReadOnly: true, VScroll: true, MaxSize: Size{Width: 32767, Height: 32767}},
		},
	}
}

// centerOnScreen replaces the CW_USEDEFAULT cascade position and keeps the
// window inside the work area of small screens.
func (u *ui) centerOnScreen() {
	var work win.RECT
	if !win.SystemParametersInfo(spiGetWorkArea, 0, unsafe.Pointer(&work), 0) {
		return
	}
	workWidth, workHeight := int(work.Right-work.Left), int(work.Bottom-work.Top)
	if workWidth <= 0 || workHeight <= 0 {
		return
	}
	b := u.mw.BoundsPixels()
	width, height := min(b.Width, workWidth), min(b.Height, workHeight)
	bounds := walk.Rectangle{
		X:      int(work.Left) + (workWidth-width)/2,
		Y:      int(work.Top) + (workHeight-height)/2,
		Width:  width,
		Height: height,
	}
	_ = u.mw.SetBoundsPixels(bounds)
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
	portChanged := u.oldPort != s.Port
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
			if err == nil && portChanged {
				u.logLine(msg("LogPortApplyHint", "RDP port changed; restart TermService to apply the new port"))
			}
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
	u.runOperation(msg("OpUninstall", "uninstall"), app.OpUninstall)
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

// runOperation executes op in the out-of-session host and streams its log into
// the log panel.
func (u *ui) runOperation(label, op string) {
	if u.opBusy || u.stopped() {
		return
	}
	u.setBusy(true)
	u.logLine("=== " + label + " ===")
	go func() {
		err := u.backend.RunOperation(op, guiWriter{u})
		u.post(func() {
			if err != nil {
				u.logLine(msg("LogOpError", "error: %s", err))
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
	if u.log == nil {
		return
	}
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
	versionColor := colorIdle
	if s.TermsrvErr != nil {
		u.lsTSVer.SetText(st("NA", "N/A"))
		u.lsSupport.SetText("")
	} else {
		u.lsTSVer.SetText(s.TermsrvVersion)
		if s.Supported {
			u.lsSupport.SetText(st("FullySupported", "[fully supported]"))
			versionColor = colorOK
		} else {
			u.lsSupport.SetText(st("NotSupported", "[not supported]"))
			versionColor = colorBad
		}
	}
	setDot(u.dotVersion, versionColor)
	if s.DBErr != nil {
		u.lsDB.SetText(s.DBErr.Error())
	} else {
		u.lsDB.SetText(s.DBUpdated)
	}
	serviceColor := colorBad
	if s.TermErr != nil {
		u.lsService.SetText(st("Unknown", "Unknown"))
		serviceColor = colorIdle
	} else {
		u.lsService.SetText(fmt.Sprintf("%s (pid %d)", termStateText(s.TermState), s.TermPID))
		if s.TermState == 4 {
			serviceColor = colorOK
		}
	}
	setDot(u.dotService, serviceColor)
	patchColor := colorBad
	switch {
	case s.TermErr != nil || s.TermState != 4:
		u.lsPatch.SetText(st("NA", "N/A"))
		patchColor = colorIdle
	case s.PatchNeedAdmin:
		u.lsPatch.SetText(st("NeedAdmin", "run as administrator"))
		patchColor = colorIdle
	case s.PatchErr != nil:
		u.lsPatch.SetText(s.PatchErr.Error())
	case s.PatchActive:
		u.lsPatch.SetText(st("Patched", "ACTIVE"))
		patchColor = colorOK
	default:
		u.lsPatch.SetText(st("NotPatched", "inactive"))
	}
	setDot(u.dotPatch, patchColor)
	svcColor := colorIdle
	switch {
	case !s.ServiceInstalled:
		u.lsSvc.SetText(st("NotInstalled", "Not installed"))
	case s.ServiceState == svc.Running:
		u.lsSvc.SetText(termStateText(uint32(s.ServiceState)))
		svcColor = colorOK
	case s.ServiceState == svc.Stopped:
		u.lsSvc.SetText(termStateText(uint32(s.ServiceState)))
		svcColor = colorBad
	default:
		u.lsSvc.SetText(termStateText(uint32(s.ServiceState)))
	}
	setDot(u.dotSvc, svcColor)
	port := fmt.Sprintf(" (%d)", s.ListenerPort)
	if s.Listener {
		u.lsListener.SetText(st("Listening", "Listening") + port)
		setDot(u.dotListener, colorOK)
	} else {
		u.lsListener.SetText(st("NotListening", "Not listening") + port)
		setDot(u.dotListener, colorBad)
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
