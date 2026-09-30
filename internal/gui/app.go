//go:build !nogui

package gui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

const (
	// discoveryInterval is the pause between background discovery rounds.
	discoveryInterval = 30 * time.Second
	// shutdownTimeout bounds how long quitting waits for sessions to stop
	// (receiver teardown, virtual audio device removal).
	shutdownTimeout = 10 * time.Second
	// clockStatsInterval is how often the PTP line of connected receivers
	// is refreshed while the popup is shown.
	clockStatsInterval = time.Second

	iconApp     = "video-television"
	iconActive  = "media-playback-playing"
	iconSpeaker = "audio-speakers"
	iconMirror  = "video-display"
	iconSwitch  = "window-duplicate"
	iconFile    = "document-open"
	iconURL     = "insert-link"
	iconStop    = "media-playback-stop"
	iconRefresh = "view-refresh"
	iconAdd     = "list-add"
	iconQuit    = "application-exit"
	iconError   = "dialog-error"

	videoFileFilter = "Videos (*.mkv *.webm *.mp4 *.m4v *.mov *.ts *.m3u8);;All Files (*)"
)

type sessionKind int

const (
	sessionMirror sessionKind = iota
	sessionPlay
	sessionAudio
)

// session is a running (or starting) mirror or playback. Fields other than
// cancel and done are only touched on the Qt main thread.
type session struct {
	receiver Receiver
	kind     sessionKind
	title    string
	status   string
	started  bool
	stopping bool
	// app is the receiver app the location was opened in, if any.
	app string
	// switchSource, when set, picks a new screen or window for a running
	// mirror; switching is true while its picker is open.
	switchSource func(context.Context) error
	switching    bool
	// clock is the latest PTP report, nil when the session has none.
	clock  *ClockStats
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// describe is the session's state as shown next to its receiver.
func (s *session) describe() string {
	switch {
	case s.stopping:
		return "Stopping…"
	case !s.started:
		return s.status
	case s.switching:
		return "Choosing what to share…"
	case s.kind == sessionMirror:
		return "Mirroring screen"
	case s.kind == sessionAudio:
		return "Playing this computer's sound"
	default:
		return "Playing " + s.title
	}
}

// app is the tray application. Its fields are only touched on the Qt main
// thread; goroutines hand results over with mainthread.Start.
type app struct {
	ctx     context.Context
	backend Backend

	receivers    *receiverList
	paired       map[string]bool
	sessions     map[string]*session
	running      sync.WaitGroup
	refresh      chan struct{}
	discovering  bool
	discoveryErr error
	lastDir      string
	// clockPolling is set while a ClockStats round is running.
	clockPolling bool
	// quitting is set once quit asked the event loop to stop.
	quitting bool

	bus         *sessionBus
	tray        *trayItem
	trayIcon    string
	trayTooltip string
	popup       *popup
}

// Run shows the tray icon and blocks until the user quits or ctx is
// cancelled. Active sessions are stopped before it returns. Without a
// system tray, the receiver list is shown as a window instead.
func Run(ctx context.Context, backend Backend) error {
	// Flags were parsed already; Qt only needs the program name.
	qt.NewQApplication([]string{os.Args[0]})
	qt.QCoreApplication_SetApplicationName("foxCast")
	qt.QGuiApplication_SetApplicationDisplayName("foxCast")
	qt.QGuiApplication_SetDesktopFileName(desktopEntry)
	// foxCast lives in the tray: closing a dialog must never end it. Qt 6
	// also quits automatically when the last QEventLoopLocker goes away
	// (file dialogs, portals and nested exec loops take them), so both
	// automatic quits are off and only quit ends the event loop.
	qt.QGuiApplication_SetQuitOnLastWindowClosed(false)
	qt.QCoreApplication_SetQuitLockEnabled(false)
	qt.QGuiApplication_SetWindowIcon(qt.QIcon_FromTheme(iconApp))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a := &app{
		ctx:       ctx,
		backend:   backend,
		receivers: newReceiverList(),
		paired:    map[string]bool{},
		sessions:  map[string]*session{},
		refresh:   make(chan struct{}, 1),
	}
	if home, err := os.UserHomeDir(); err == nil {
		a.lastDir = home
		if videos := filepath.Join(home, "Videos"); isDir(videos) {
			a.lastDir = videos
		}
	}

	var err error
	if a.bus, err = connectSessionBus(); err != nil {
		log.Printf("session bus: %v", err)
	} else {
		defer a.bus.close()
		a.trayIcon, a.trayTooltip = iconApp, "foxCast"
		a.tray, err = a.bus.newTrayItem(a.trayIcon, a.trayTooltip, func(x, y int32, token string) {
			mainthread.Start(func() { a.popup.toggleAt(x, y, token) })
		})
		if err != nil {
			log.Printf("tray icon: %v", err)
			a.tray = nil
		}
	}
	a.popup = newPopup(a, a.tray != nil)
	if a.tray == nil {
		log.Println("no system tray available; showing the foxCast window")
		a.popup.present()
	}
	a.discovering = true
	a.update()

	go a.discoverLoop()
	clockTimer := qt.NewQTimer()
	clockTimer.OnTimeout(a.pollClockStats)
	clockTimer.Start(int(clockStatsInterval / time.Millisecond))
	go func() {
		<-ctx.Done()
		mainthread.Start(a.quit)
	}()
	for {
		qt.QApplication_Exec()
		if a.quitting {
			break
		}
		log.Println("warning: Qt stopped the event loop on its own; resuming")
	}

	// The event loop has stopped, so session callbacks posted from here on
	// never run; sessions only need their contexts cancelled.
	cancel()
	stopped := make(chan struct{})
	go func() {
		a.running.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(shutdownTimeout):
		log.Println("warning: sessions did not stop in time")
	}
	return nil
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// quit ends the application. The event loop only stops for good through
// here; see Run.
func (a *app) quit() {
	a.quitting = true
	qt.QCoreApplication_Quit()
}

// discoverLoop browses for receivers until the application stops, a round at
// a time, rerunning early when requestRefresh is called.
func (a *app) discoverLoop() {
	for {
		devices, err := a.backend.Discover(a.ctx)
		if a.ctx.Err() != nil {
			return
		}
		now := time.Now()
		mainthread.Start(func() {
			a.discovering = false
			a.discoveryErr = err
			if err != nil {
				log.Printf("discovery: %v", err)
			}
			a.receivers.update(devices, now)
			a.refreshPaired()
			a.update()
		})
		select {
		case <-a.ctx.Done():
			return
		case <-a.refresh:
		case <-time.After(discoveryInterval):
		}
		mainthread.Start(func() {
			a.discovering = true
			a.update()
		})
	}
}

// pollClockStats fetches the PTP reports of running sessions while the popup
// is shown, updating their rows in place.
func (a *app) pollClockStats() {
	if a.clockPolling || !a.popup.w.IsVisible() {
		return
	}
	var running []*session
	for _, s := range a.sessions {
		if s.started && !s.stopping {
			running = append(running, s)
		}
	}
	if len(running) == 0 {
		return
	}
	a.clockPolling = true
	go func() {
		reports := make([]*ClockStats, len(running))
		for i, s := range running {
			if st, ok := a.backend.ClockStats(&s.receiver); ok {
				reports[i] = &st
			}
		}
		mainthread.Start(func() {
			a.clockPolling = false
			now := time.Now()
			for i, s := range running {
				s.clock = reports[i]
				a.popup.showClock(s, now)
			}
		})
	}()
}

// requestRefresh starts a discovery round now (or right after the current
// one).
func (a *app) requestRefresh() {
	select {
	case a.refresh <- struct{}{}:
	default:
	}
	a.discovering = true
	a.update()
}

// refreshPaired reloads which listed receivers have saved pairings.
func (a *app) refreshPaired() {
	ids := a.receivers.deviceIDs()
	go func() {
		paired := a.backend.Paired(ids)
		mainthread.Start(func() {
			a.paired = paired
			a.update()
		})
	}()
}

func (a *app) isPaired(r *Receiver) bool {
	return r.DeviceID != "" && a.paired[r.DeviceID]
}

// update brings the tray and popup in line with the current state.
func (a *app) update() {
	if a.tray != nil {
		a.updateTray()
	}
	a.popup.update()
}

// session returns the receiver's session, if any.
func (a *app) session(r *Receiver) *session {
	return a.sessions[r.Key()]
}

// dialogParent prepares for showing a dialog and returns its parent. The
// tray popup closes (as applet popups do), so dialogs stand alone.
func (a *app) dialogParent() *qt.QWidget {
	if a.popup.anchored {
		a.popup.hide()
		return nil
	}
	return a.popup.w
}

func (a *app) mirror(r Receiver) {
	a.start(r, sessionMirror, "")
}

func (a *app) streamAudio(r Receiver) {
	a.start(r, sessionAudio, "")
}

func (a *app) playFile(r Receiver) {
	file := qt.QFileDialog_GetOpenFileName4(a.dialogParent(), "Play Video on "+r.Name, a.lastDir, videoFileFilter)
	if file == "" {
		return
	}
	a.lastDir = filepath.Dir(file)
	a.start(r, sessionPlay, file)
}

func (a *app) playURL(r Receiver) {
	if location, ok := a.askMedia(r); ok {
		a.start(r, sessionPlay, location)
	}
}

func (a *app) stop(r Receiver) {
	s := a.session(&r)
	if s == nil || s.stopping {
		return
	}
	s.stopping = true
	s.cancel()
	a.update()
}

// canSwitchSource reports whether s can move to another screen or window now.
func (s *session) canSwitchSource() bool {
	return s != nil && s.switchSource != nil && !s.switching && !s.stopping
}

// switchSource lets the user pick a new screen or window for r's mirror.
func (a *app) switchSource(r Receiver) {
	s := a.session(&r)
	if !s.canSwitchSource() {
		return
	}
	// Keep the popup from covering the portal's picker.
	if a.popup.anchored {
		a.popup.hide()
	}
	s.switching = true
	a.update()
	go func() {
		err := s.switchSource(s.ctx)
		mainthread.Start(func() {
			s.switching = false
			a.update()
			if err != nil && s.ctx.Err() == nil && !errors.Is(err, sender.ErrPortalCancelled) {
				log.Printf("%s: %v", s.receiver.Name, err)
				a.notify(iconError, "Could not change what is shared with "+s.receiver.Name, errorText(err))
			}
		})
	}()
}

// forget deletes r's saved pairing.
func (a *app) forget(r Receiver) {
	if r.DeviceID == "" {
		return
	}
	go func() {
		err := a.backend.Forget(r.DeviceID)
		mainthread.Start(func() {
			if err != nil {
				a.notify(iconError, "Could not forget "+r.Name, errorText(err))
			}
			a.refreshPaired()
		})
	}()
}

// start begins a session on r, replacing (after stopping) any session it
// already has.
func (a *app) start(r Receiver, kind sessionKind, location string) {
	key := r.Key()
	previous := a.sessions[key]
	if previous != nil {
		previous.stopping = true
		previous.cancel()
	}
	ctx, cancel := context.WithCancel(a.ctx)
	s := &session{
		receiver: r,
		kind:     kind,
		title:    mediaTitle(location),
		status:   "Starting…",
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	a.sessions[key] = s
	a.update()

	status := func(msg string) {
		mainthread.Start(func() {
			s.status = msg
			a.update()
		})
	}
	cb := Callbacks{
		Status: status,
		Connected: func(name, model, deviceID string) {
			mainthread.Start(func() {
				if a.receivers.identify(key, name, model, deviceID) {
					s.receiver.Name, s.receiver.Model, s.receiver.DeviceID = name, model, deviceID
					a.update()
				}
			})
		},
		Started: func() {
			mainthread.Start(func() {
				s.started = true
				a.update()
				// Pairing may just have completed.
				a.refreshPaired()
				switch kind {
				case sessionMirror:
					a.notify(iconMirror, "Mirroring to "+s.receiver.Name, "Your screen is being shown on "+s.receiver.Name+".")
				case sessionAudio:
					a.notify(iconSpeaker, "Playing on "+s.receiver.Name, "Your computer's sound is playing on "+s.receiver.Name+".")
				default:
					a.notify(iconActive, "Playing on "+s.receiver.Name, s.title)
				}
			})
		},
		OpenedInApp: func(app string) {
			mainthread.Start(func() {
				s.started, s.app = true, app
				a.refreshPaired()
				a.notify(iconActive, "Opened on "+s.receiver.Name, s.title+" is open in "+app+".")
			})
		},
	}
	if kind == sessionMirror {
		cb.SourceSwitchable = func(switchSource func(context.Context) error) {
			mainthread.Start(func() {
				s.switchSource = switchSource
				a.update()
			})
		}
	}
	prompt := func(ctx context.Context, receiver string, kind CredentialKind) (string, error) {
		status("Waiting for pairing code…")
		value, err := a.prompt(ctx, receiver, kind)
		status("Pairing…")
		return value, err
	}

	a.running.Add(1)
	go func() {
		defer a.running.Done()
		defer close(s.done)
		defer cancel()
		if previous != nil {
			<-previous.done
		}
		var err error
		if ctx.Err() == nil {
			switch kind {
			case sessionMirror:
				err = a.backend.Mirror(ctx, &s.receiver, prompt, cb)
			case sessionAudio:
				err = a.backend.StreamAudio(ctx, &s.receiver, prompt, cb)
			default:
				err = a.backend.Play(ctx, &s.receiver, location, prompt, cb)
			}
		}
		stopped := ctx.Err() != nil
		mainthread.Start(func() { a.finished(s, err, stopped) })
	}()
}

// finished records the end of session s.
func (a *app) finished(s *session, err error, stopped bool) {
	key := s.receiver.Key()
	if a.sessions[key] == s {
		delete(a.sessions, key)
	}
	a.refreshPaired()
	a.update()
	name := s.receiver.Name
	switch {
	case err != nil && !stopped && !errors.Is(err, ErrCancelled) && !errors.Is(err, sender.ErrPortalCancelled):
		log.Printf("%s: %v", name, err)
		what := "Mirroring to " + name + " failed"
		if s.kind != sessionMirror {
			what = "Playing on " + name + " failed"
		}
		a.notify(iconError, what, errorText(err))
	case err == nil && s.app != "":
		// Nothing to report: the app plays on its own.
	case err == nil && !stopped && s.started && s.kind == sessionPlay:
		a.notify(iconApp, "Playback finished", s.title+" finished playing on "+name+".")
	case err == nil && !stopped && s.started && s.kind == sessionAudio:
		a.notify(iconSpeaker, "Sound stopped", name+" stopped playing this computer's sound.")
	case err == nil && !stopped && s.started:
		a.notify(iconApp, "Mirroring stopped", name+" ended the mirroring session.")
	}
}

// errorText is err for display: its message, capitalized.
func errorText(err error) string {
	msg := err.Error()
	if msg == "" {
		return "Unknown error"
	}
	if c := msg[0]; c >= 'a' && c <= 'z' {
		msg = string(c-'a'+'A') + msg[1:]
	}
	return msg
}

// notify shows a desktop notification, falling back to a message box for
// errors when there is no notification service.
func (a *app) notify(icon, title, message string) {
	if a.bus != nil {
		a.bus.notify(icon, title, message)
		return
	}
	if icon == iconError {
		qt.QMessageBox_Warning(a.dialogParent(), title, message)
	}
}

// prompt asks for a pairing credential from a session goroutine. It gives up
// (closing the dialog) when ctx is cancelled.
func (a *app) prompt(ctx context.Context, receiver string, kind CredentialKind) (string, error) {
	type answer struct {
		value string
		ok    bool
	}
	answers := make(chan answer, 1)
	var dismiss func()
	mainthread.Start(func() {
		dismiss = showCredentialDialog(a.dialogParent(), receiver, kind, func(value string, ok bool) {
			answers <- answer{value, ok}
		})
	})
	select {
	case ans := <-answers:
		if !ans.ok {
			return "", ErrCancelled
		}
		return ans.value, nil
	case <-ctx.Done():
		mainthread.Start(func() {
			if dismiss != nil {
				dismiss()
			}
		})
		return "", ctx.Err()
	}
}

// updateTray updates the tray icon, tooltip and context menu.
func (a *app) updateTray() {
	receivers := a.receivers.receivers()

	tooltip := "foxCast"
	icon := iconApp
	var active []string
	for _, r := range receivers {
		if s := a.session(&r); s != nil && s.started && !s.stopping {
			active = append(active, s.describe()+" on "+r.Name)
		}
	}
	switch {
	case len(active) == 1:
		tooltip, icon = active[0], iconActive
	case len(active) > 1:
		tooltip, icon = fmt.Sprintf("%d active sessions", len(active)), iconActive
	case len(receivers) == 1:
		tooltip = "1 AirPlay receiver"
	case len(receivers) > 1:
		tooltip = fmt.Sprintf("%d AirPlay receivers", len(receivers))
	}
	if icon != a.trayIcon || tooltip != a.trayTooltip {
		a.trayIcon, a.trayTooltip = icon, tooltip
		a.tray.setIcon(icon, tooltip)
	}

	// Menu actions arrive on the D-Bus goroutine.
	onMain := func(f func()) func() { return func() { mainthread.Start(f) } }
	var items []trayMenuItem
	for _, r := range receivers {
		s := a.session(&r)
		switch {
		case s != nil && !s.stopping:
			text := "Stop Mirroring to " + r.Name
			if s.kind != sessionMirror {
				text = "Stop Playing on " + r.Name
			}
			items = append(items, trayMenuItem{label: text, icon: iconStop, action: onMain(func() { a.stop(r) })})
			if s.switchSource != nil {
				items = append(items, trayMenuItem{label: "Change What's Shared with " + r.Name + "…", icon: iconSwitch, disabled: !s.canSwitchSource(), action: onMain(func() { a.switchSource(r) })})
			}
		case s == nil && a.isPaired(&r) && r.CanMirror():
			items = append(items, trayMenuItem{label: "Mirror to " + r.Name, icon: iconMirror, action: onMain(func() { a.mirror(r) })})
		case s == nil && a.isPaired(&r) && r.CanStreamAudio() && !r.CanPlay():
			items = append(items, trayMenuItem{label: "Play Sound on " + r.Name, icon: iconSpeaker, action: onMain(func() { a.streamAudio(r) })})
		}
	}
	if len(items) > 0 {
		items = append(items, trayMenuItem{separator: true})
	}
	items = append(items,
		trayMenuItem{label: "Show Receivers", icon: iconApp, action: onMain(func() { a.popup.toggleAt(0, 0, "") })},
		trayMenuItem{label: "Search Again", icon: iconRefresh, disabled: a.discovering, action: onMain(a.requestRefresh)},
		trayMenuItem{label: "Add Receiver by Address…", icon: iconAdd, action: onMain(a.addReceiver)},
		trayMenuItem{separator: true},
		trayMenuItem{label: "Quit", icon: iconQuit, action: onMain(a.quit)},
	)
	a.tray.setMenu(items)
}

// receiverIcon is the theme icon for r.
func receiverIcon(r *Receiver) string {
	if !r.CanMirror() && !r.CanPlay() {
		return iconSpeaker
	}
	return iconApp
}

// addReceiver asks for a receiver address and lists it.
func (a *app) addReceiver() {
	var ok bool
	text := qt.QInputDialog_GetText4(a.dialogParent(), "Add Receiver",
		"Host name or IP address of the AirPlay receiver (optionally with :port):",
		qt.QLineEdit__Normal, "", &ok)
	if !ok {
		return
	}
	host, port, err := parseAddress(text)
	if err != nil {
		qt.QMessageBox_Warning(a.dialogParent(), "Add Receiver", errorText(err))
		return
	}
	r := a.receivers.addManual(host, port)
	a.popup.expanded = r.Key()
	a.update()
	a.popup.present()
}
