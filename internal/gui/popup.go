//go:build !nogui

package gui

import (
	"fmt"
	"html"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"
)

const (
	popupWidth  = 440
	popupHeight = 520
	// popupReopenGuard: clicking the tray icon while the popup is open
	// first moves focus away, which closes the popup; the click itself,
	// arriving right after, must not reopen it.
	popupReopenGuard = 400 * time.Millisecond
)

// popup is the receiver list, laid out like Plasma's Networks applet: paired
// receivers first, the rest below, each row expanding to show its actions.
// With a tray icon it opens against the panel like an applet popup and
// closes when it loses focus; without one it is an ordinary window.
type popup struct {
	app      *app
	w        *qt.QWidget
	anchored bool

	search  *qt.QLineEdit
	refresh *qt.QToolButton
	status  *qt.QLabel
	scroll  *qt.QScrollArea
	list    *qt.QVBoxLayout

	// clocks are the PTP lines of the listed sessions, by receiver key.
	clocks map[string]*qt.QLabel
	// rows are the listed receivers' rows, by receiver key.
	rows map[string]*qt.QWidget
	// anchorKey's row is kept anchorOffset pixels below the top of the
	// view across rebuilds; settle reapplies that once Qt has laid out
	// the new rows.
	anchorKey    string
	anchorOffset int
	settle       *qt.QTimer

	expanded    string
	hiddenAt    time.Time
	lastX       int32
	lastY       int32
	layerShell  bool
	rowStyle    string
	activeStyle string
}

func newPopup(a *app, anchored bool) *popup {
	p := &popup{app: a, anchored: anchored, layerShell: anchored && qt.QGuiApplication_PlatformName() == "wayland"}
	p.w = qt.NewQWidget2()
	p.w.SetWindowTitle("foxCast")
	if anchored {
		p.w.SetWindowFlags(qt.Tool | qt.FramelessWindowHint | qt.WindowStaysOnTopHint)
		p.w.SetFixedSize2(popupWidth, popupHeight)
	} else {
		p.w.Resize(popupWidth, popupHeight)
	}

	hl := p.w.Palette().ColorWithCr(qt.QPalette__Highlight)
	tint := func(alpha float64) string {
		return fmt.Sprintf("rgba(%d, %d, %d, %.2f)", hl.Red(), hl.Green(), hl.Blue(), alpha)
	}
	// ".QFrame" matches the row frames only, not labels (QLabel is a QFrame).
	p.rowStyle = ".QFrame { border: 1px solid transparent; border-radius: 4px; }" +
		".QFrame:hover { background: " + tint(0.15) + "; border-color: " + tint(0.5) + "; }"
	p.activeStyle = ".QFrame { background: " + tint(0.10) + "; border: 1px solid " + tint(0.4) + "; border-radius: 4px; }"

	// As a popup, draw the frame a window manager would otherwise provide.
	body := p.w
	if anchored {
		outer := qt.NewQVBoxLayout(p.w)
		outer.SetContentsMargins(0, 0, 0, 0)
		frame := qt.NewQFrame2()
		frame.SetFrameShape(qt.QFrame__StyledPanel)
		outer.AddWidget(frame.QWidget)
		body = frame.QWidget
	}
	layout := qt.NewQVBoxLayout(body)
	layout.SetContentsMargins(8, 8, 8, 8)
	layout.SetSpacing(6)

	header := qt.NewQHBoxLayout2()
	p.search = qt.NewQLineEdit2()
	p.search.SetPlaceholderText("Search…")
	p.search.SetClearButtonEnabled(true)
	p.search.OnTextChanged(func(string) { p.update() })
	header.AddWidget2(p.search.QWidget, 1)
	p.refresh = qt.NewQToolButton2()
	p.refresh.SetIcon(qt.QIcon_FromTheme(iconRefresh))
	p.refresh.SetToolTip("Search again")
	p.refresh.SetAutoRaise(true)
	p.refresh.OnClicked(a.requestRefresh)
	header.AddWidget(p.refresh.QWidget)
	more := qt.NewQToolButton2()
	more.SetIcon(qt.QIcon_FromTheme("application-menu"))
	more.SetToolTip("More")
	more.SetAutoRaise(true)
	more.SetPopupMode(qt.QToolButton__InstantPopup)
	menu := qt.NewQMenu(more.QWidget)
	menu.AddAction2(qt.QIcon_FromTheme(iconAdd), "Add Receiver by Address…").OnTriggered(a.addReceiver)
	menu.AddSeparator()
	menu.AddAction2(qt.QIcon_FromTheme(iconQuit), "Quit foxCast").OnTriggered(a.quit)
	more.SetMenu(menu)
	header.AddWidget(more.QWidget)
	layout.AddLayout(header.QLayout)

	p.status = qt.NewQLabel2()
	p.status.SetEnabled(false)
	p.status.SetIndent(4)
	layout.AddWidget(p.status.QWidget)

	scroll := qt.NewQScrollArea2()
	scroll.SetWidgetResizable(true)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	scroll.SetHorizontalScrollBarPolicy(qt.ScrollBarAlwaysOff)
	content := qt.NewQWidget2()
	p.list = qt.NewQVBoxLayout(content)
	p.list.SetContentsMargins(0, 0, 0, 0)
	p.list.SetSpacing(2)
	scroll.SetWidget(content)
	layout.AddWidget2(scroll.QWidget, 1)
	p.scroll = scroll
	p.settle = qt.NewQTimer2(p.w.QObject)
	p.settle.SetSingleShot(true)
	p.settle.OnTimeout(p.scrollToAnchor)

	p.w.OnKeyPressEvent(func(super func(*qt.QKeyEvent), event *qt.QKeyEvent) {
		if p.anchored && event.Key() == int(qt.Key_Escape) {
			p.hide()
			return
		}
		super(event)
	})
	p.w.OnChangeEvent(func(super func(*qt.QEvent), event *qt.QEvent) {
		super(event)
		if p.anchored && event.Type() == qt.QEvent__ActivationChange && p.w.IsVisible() && !p.w.IsActiveWindow() {
			p.hide()
		}
	})
	p.w.OnCloseEvent(func(super func(*qt.QCloseEvent), event *qt.QCloseEvent) {
		if p.anchored {
			event.Ignore()
			p.hide()
			return
		}
		super(event)
		a.quit()
	})
	return p
}

func (p *popup) hide() {
	if p.w.IsVisible() {
		p.w.Hide()
		p.hiddenAt = time.Now()
	}
}

// toggleAt opens the popup for a tray click at global (x, y), or closes it.
func (p *popup) toggleAt(x, y int32, activationToken string) {
	if !p.anchored {
		p.present()
		return
	}
	if p.w.IsVisible() {
		p.hide()
		return
	}
	if time.Since(p.hiddenAt) < popupReopenGuard {
		return
	}
	if activationToken != "" {
		// Qt's Wayland backend activates the next window it maps with it.
		_ = os.Setenv("XDG_ACTIVATION_TOKEN", activationToken)
	}
	p.lastX, p.lastY = x, y
	p.present()
}

// present shows the popup: next to the last tray click, or for an ordinary
// window, wherever the window manager puts it.
func (p *popup) present() {
	if p.anchored {
		p.place(p.lastX, p.lastY)
	}
	// Open at the top, where receivers in use are listed.
	if !p.w.IsVisible() {
		p.scroll.VerticalScrollBar().SetValue(0)
	}
	p.w.Show()
	p.w.Raise()
	p.w.ActivateWindow()
}

// place positions the hidden popup for a click at (x, y). Without a click
// position (0, 0) it goes to the primary screen's bottom right corner, where
// the tray usually is.
func (p *popup) place(x, y int32) {
	screen := qt.QGuiApplication_PrimaryScreen()
	if x != 0 || y != 0 {
		if s := qt.QGuiApplication_ScreenAt(qt.NewQPoint2(int(x), int(y))); s != nil {
			screen = s
		}
	}
	if screen == nil {
		return
	}
	// Layer-shell margins are relative to the area other surfaces (panels)
	// leave free, so the popup is placed within the full screen and the
	// compositor keeps it clear of the panel. Elsewhere the popup is moved
	// within the available area directly.
	g := screen.AvailableGeometry()
	if p.layerShell {
		g = screen.Geometry()
	}
	area := rect{x: g.X(), y: g.Y(), w: g.Width(), h: g.Height()}
	if x == 0 && y == 0 {
		x, y = int32(area.x+area.w), int32(area.y+area.h)
	}
	pl := popupPlacement(int(x), int(y), area, popupWidth, popupHeight)
	if p.layerShell {
		p.w.WinId() // create the QWindow before its first show
		layerShellPlace(p.w.WindowHandle(), screen, pl)
		return
	}
	p.w.Move(pl.x, pl.y)
}

// update rebuilds the listing from the application state.
func (p *popup) update() {
	a := p.app
	all := a.receivers.receivers()
	query := p.search.Text()
	active, known, other := groupReceivers(all, a.isPaired, func(r *Receiver) bool { return a.session(r) != nil }, query)

	switch {
	case a.discoveryErr != nil && len(all) == 0:
		p.status.SetText("Discovery failed: " + errorText(a.discoveryErr))
	case a.discovering:
		p.status.SetText("Searching for AirPlay receivers…")
	case len(all) == 1:
		p.status.SetText("1 AirPlay receiver")
	default:
		p.status.SetText(fmt.Sprintf("%d AirPlay receivers", len(all)))
	}
	p.refresh.SetEnabled(!a.discovering)

	p.pickAnchor()
	// Old rows are deleted later: this may run from one of their buttons.
	for p.list.Count() > 0 {
		item := p.list.TakeAt(0)
		if w := item.Widget(); w != nil {
			w.Hide()
			w.DeleteLater()
		}
	}

	p.clocks = map[string]*qt.QLabel{}
	p.rows = map[string]*qt.QWidget{}
	for _, section := range []struct {
		title     string
		receivers []Receiver
	}{
		{"Connected", active},
		{"Paired", known},
		{"Available", other},
	} {
		if len(section.receivers) == 0 {
			continue
		}
		// A list of only unpaired receivers needs no heading.
		if len(section.receivers) != len(active)+len(known)+len(other) || section.title != "Available" {
			p.addSection(section.title)
		}
		for _, r := range section.receivers {
			p.addRow(r, a.isPaired(&r))
		}
	}
	switch {
	case len(all) == 0:
		text := "<p><b>No AirPlay receivers found</b></p>" +
			"<p>Make sure the Apple TV is switched on and on the same network as this computer, or add it by address from the menu.</p>"
		if a.discovering {
			text = "<p><b>Searching for AirPlay receivers…</b></p>"
		}
		p.addPlaceholder(text)
	case len(active)+len(known)+len(other) == 0:
		p.addPlaceholder("<p>No receivers match “" + html.EscapeString(strings.TrimSpace(query)) + "”.</p>")
	}
	p.list.AddStretch()
	p.keepAnchor()
}

// pickAnchor chooses the row to hold still while the listing is rebuilt, so
// rows moving between sections (a receiver connecting or disconnecting) do
// not shift what is on screen: the row under the mouse, else the expanded
// one, else the topmost one in view.
func (p *popup) pickAnchor() {
	p.anchorKey = ""
	if !p.w.IsVisible() {
		return
	}
	top := p.scroll.VerticalScrollBar().Value()
	anchor := ""
	for key, row := range p.rows {
		if row.UnderMouse() {
			anchor = key
		}
	}
	if anchor == "" && p.rows[p.expanded] != nil {
		anchor = p.expanded
	}
	if anchor == "" {
		for key, row := range p.rows {
			if row.Y()+row.Height() > top && (anchor == "" || row.Y() < p.rows[anchor].Y()) {
				anchor = key
			}
		}
	}
	if anchor != "" {
		p.anchorKey, p.anchorOffset = anchor, p.rows[anchor].Y()-top
	}
}

// keepAnchor scrolls the rebuilt listing back to the anchor row: at once, by
// laying the rows out now, and again after Qt's own layout pass.
func (p *popup) keepAnchor() {
	if p.anchorKey == "" {
		return
	}
	content := p.scroll.Widget()
	viewport := p.scroll.Viewport()
	p.list.Activate()
	content.Resize(viewport.Width(), max(viewport.Height(), content.MinimumSizeHint().Height()))
	p.scrollToAnchor()
	p.settle.Start(0)
}

func (p *popup) scrollToAnchor() {
	if row := p.rows[p.anchorKey]; row != nil {
		p.scroll.VerticalScrollBar().SetValue(row.Y() - p.anchorOffset)
	}
}

// add appends w to the listing. Widgets added to a shown parent are only
// shown on the next event loop pass; showing them now avoids a blank frame.
func (p *popup) add(w *qt.QWidget) {
	p.list.AddWidget(w)
	w.Show()
}

func (p *popup) addSection(title string) {
	label := qt.NewQLabel3(title)
	font := qt.NewQFont5(label.Font())
	font.SetBold(true)
	label.SetFont(font)
	label.SetEnabled(false)
	label.SetIndent(4)
	label.SetContentsMargins(0, 6, 0, 2)
	p.add(label.QWidget)
}

func (p *popup) addPlaceholder(text string) {
	label := qt.NewQLabel2()
	label.SetTextFormat(qt.RichText)
	label.SetText(text)
	label.SetWordWrap(true)
	label.SetAlignment(qt.AlignCenter)
	label.SetContentsMargins(16, 32, 16, 32)
	p.add(label.QWidget)
}

// addRow adds r's row: icon, name, state and a primary action button, and,
// when expanded, the full set of actions and details.
func (p *popup) addRow(r Receiver, paired bool) {
	a := p.app
	s := a.session(&r)
	key := r.Key()
	expanded := p.expanded == key
	usable := r.Usable()

	row := qt.NewQFrame2()
	row.SetAttribute(qt.WA_Hover)
	if expanded || s != nil {
		row.SetStyleSheet(p.activeStyle)
	} else {
		row.SetStyleSheet(p.rowStyle)
	}
	row.SetToolTip(receiverToolTip(&r))
	layout := qt.NewQVBoxLayout(row.QWidget)
	layout.SetContentsMargins(8, 6, 8, 6)

	top := qt.NewQHBoxLayout2()
	top.SetSpacing(10)
	icon := qt.NewQLabel2()
	iconName := receiverIcon(&r)
	if s != nil && s.started {
		iconName = iconActive
	}
	mode := qt.QIcon__Normal
	if !usable {
		mode = qt.QIcon__Disabled
	}
	icon.SetPixmap(qt.QIcon_FromTheme(iconName).Pixmap7(32, 32, mode))
	top.AddWidget(icon.QWidget)

	texts := qt.NewQVBoxLayout2()
	texts.SetSpacing(0)
	name := qt.NewQLabel3(r.Name)
	if s != nil {
		font := qt.NewQFont5(name.Font())
		font.SetBold(true)
		name.SetFont(font)
	}
	name.SetEnabled(usable)
	texts.AddWidget(name.QWidget)
	state := qt.NewQLabel3(rowState(&r, s, paired))
	state.SetEnabled(false)
	texts.AddWidget(state.QWidget)
	if s != nil {
		clock := qt.NewQLabel2()
		font := qt.NewQFont5(clock.Font())
		font.SetPointSizeF(font.PointSizeF() * 0.85)
		clock.SetFont(font)
		clock.SetEnabled(false)
		clock.SetToolTip("Clock synchronization with " + r.Name + ": round-trip latency and its jitter")
		texts.AddWidget(clock.QWidget)
		p.clocks[key] = clock
		p.setClock(clock, s, time.Now())
	}
	top.AddLayout2(texts.QLayout, 1)

	switch {
	case s != nil:
		if s.switchSource != nil {
			change := qt.NewQPushButton4(qt.QIcon_FromTheme(iconSwitch), "Change…")
			change.SetToolTip("Choose a different screen or window to share")
			change.SetEnabled(s.canSwitchSource())
			change.OnClicked(func() { a.switchSource(r) })
			top.AddWidget(change.QWidget)
		}
		stop := qt.NewQPushButton4(qt.QIcon_FromTheme(iconStop), "Stop")
		stop.SetEnabled(!s.stopping)
		stop.OnClicked(func() { a.stop(r) })
		top.AddWidget(stop.QWidget)
	case r.CanMirror():
		mirror := qt.NewQPushButton4(qt.QIcon_FromTheme(iconMirror), "Mirror")
		mirror.OnClicked(func() { a.mirror(r) })
		top.AddWidget(mirror.QWidget)
	case r.CanPlay():
		play := qt.NewQPushButton4(qt.QIcon_FromTheme(iconFile), "Play…")
		play.OnClicked(func() { a.playFile(r) })
		top.AddWidget(play.QWidget)
	case r.CanStreamAudio():
		sound := qt.NewQPushButton4(qt.QIcon_FromTheme(iconSpeaker), "Play Sound")
		sound.SetToolTip("Play this computer's sound on " + r.Name)
		sound.OnClicked(func() { a.streamAudio(r) })
		top.AddWidget(sound.QWidget)
	}
	layout.AddLayout(top.QLayout)

	if expanded {
		layout.AddWidget(p.details(r, paired))
	}
	row.OnMouseReleaseEvent(func(super func(*qt.QMouseEvent), event *qt.QMouseEvent) {
		if p.expanded == key {
			p.expanded = ""
		} else {
			p.expanded = key
		}
		p.update()
	})
	p.rows[key] = row.QWidget
	p.add(row.QWidget)
}

// showClock refreshes the PTP line of s's row, if it is listed.
func (p *popup) showClock(s *session, now time.Time) {
	if clock := p.clocks[s.receiver.Key()]; clock != nil {
		p.setClock(clock, s, now)
	}
}

// setClock shows s's PTP report in label, or hides it without one.
func (p *popup) setClock(label *qt.QLabel, s *session, now time.Time) {
	if s.clock == nil || !s.started || s.stopping {
		label.SetVisible(false)
		return
	}
	if text := clockStatsText(*s.clock, now); label.Text() != text {
		label.SetText(text)
	}
	label.SetVisible(true)
}

// details is the expanded part of r's row.
func (p *popup) details(r Receiver, paired bool) *qt.QWidget {
	a := p.app
	box := qt.NewQWidget2()
	layout := qt.NewQVBoxLayout(box)
	layout.SetContentsMargins(42, 4, 0, 0)

	if r.Usable() {
		actions := qt.NewQHBoxLayout2()
		mirror := qt.NewQPushButton4(qt.QIcon_FromTheme(iconMirror), "Mirror Screen")
		mirror.SetEnabled(r.CanMirror())
		mirror.OnClicked(func() { a.mirror(r) })
		actions.AddWidget(mirror.QWidget)
		file := qt.NewQPushButton4(qt.QIcon_FromTheme(iconFile), "Play File…")
		file.SetEnabled(r.CanPlay())
		file.OnClicked(func() { a.playFile(r) })
		actions.AddWidget(file.QWidget)
		url := qt.NewQPushButton4(qt.QIcon_FromTheme(iconURL), "Play URL…")
		url.SetEnabled(r.CanPlay())
		url.OnClicked(func() { a.playURL(r) })
		actions.AddWidget(url.QWidget)
		sound := qt.NewQPushButton4(qt.QIcon_FromTheme(iconSpeaker), "Play Sound")
		sound.SetToolTip("Play this computer's sound, without video, on " + r.Name)
		sound.SetEnabled(r.CanStreamAudio())
		sound.OnClicked(func() { a.streamAudio(r) })
		actions.AddWidget(sound.QWidget)
		layout.AddLayout(actions.QLayout)
	} else {
		note := qt.NewQLabel3("This receiver does not accept anything foxCast can send.")
		note.SetWordWrap(true)
		note.SetEnabled(false)
		layout.AddWidget(note.QWidget)
	}

	footer := qt.NewQHBoxLayout2()
	var info []string
	if r.Model != "" {
		info = append(info, r.Model)
	}
	info = append(info, net.JoinHostPort(r.IP, strconv.Itoa(r.Port)))
	address := qt.NewQLabel3(strings.Join(info, " · "))
	address.SetEnabled(false)
	address.SetTextInteractionFlags(qt.TextSelectableByMouse)
	footer.AddWidget2(address.QWidget, 1)
	if paired {
		forget := qt.NewQPushButton4(qt.QIcon_FromTheme("edit-delete-remove"), "Forget")
		forget.SetToolTip("Delete the saved pairing and password for " + r.Name)
		forget.SetFlat(true)
		forget.OnClicked(func() { a.forget(r) })
		footer.AddWidget(forget.QWidget)
	}
	layout.AddLayout(footer.QLayout)
	return box
}

// rowState is the second line of r's row.
func rowState(r *Receiver, s *session, paired bool) string {
	switch {
	case s != nil:
		return s.describe()
	case !r.Usable():
		return "Not supported"
	case !r.CanMirror() && !r.CanPlay() && paired:
		return "Paired · Speaker"
	case !r.CanMirror() && !r.CanPlay():
		return "Speaker"
	case paired && r.Model != "":
		return "Paired · " + r.Model
	case paired:
		return "Paired"
	case r.Model != "":
		return r.Model
	case r.Manual:
		return "Added by address"
	default:
		return r.IP
	}
}

func receiverToolTip(r *Receiver) string {
	lines := []string{"<b>" + html.EscapeString(r.Name) + "</b>"}
	if r.Model != "" {
		lines = append(lines, "Model: "+html.EscapeString(r.Model))
	}
	lines = append(lines, "Address: "+html.EscapeString(net.JoinHostPort(r.IP, strconv.Itoa(r.Port))))
	if r.DeviceID != "" {
		lines = append(lines, "Device ID: "+html.EscapeString(r.DeviceID))
	}
	return strings.Join(lines, "<br>")
}
