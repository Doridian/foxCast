//go:build gui

package gui

// The tray icon is a StatusNotifierItem (the protocol Plasma's system tray
// speaks) implemented directly on D-Bus rather than through
// QSystemTrayIcon: Plasma passes the click position to Activate, which the
// popup needs to open against the panel, and Qt discards it.

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const (
	sniPath            = "/StatusNotifierItem"
	sniInterface       = "org.kde.StatusNotifierItem"
	watcherName        = "org.kde.StatusNotifierWatcher"
	watcherPath        = "/StatusNotifierWatcher"
	watcherInterface   = "org.kde.StatusNotifierWatcher"
	menuPath           = "/MenuBar"
	menuInterface      = "com.canonical.dbusmenu"
	notificationsName  = "org.freedesktop.Notifications"
	notificationsPath  = "/org/freedesktop/Notifications"
	notificationsIface = "org.freedesktop.Notifications"

	// desktopEntry names foxcast.desktop, for notification grouping.
	desktopEntry = "foxcast"
)

// errNoTrayHost means no StatusNotifierWatcher is running.
var errNoTrayHost = errors.New("no StatusNotifierItem host (system tray) on the session bus")

// sessionBus is foxCast's session bus connection: notifications and, when a
// tray host is running, the tray icon.
type sessionBus struct {
	conn *dbus.Conn
}

func connectSessionBus() (*sessionBus, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	return &sessionBus{conn: conn}, nil
}

func (b *sessionBus) close() {
	_ = b.conn.Close()
}

// notify shows a desktop notification without waiting for the server.
func (b *sessionBus) notify(icon, summary, body string) {
	hints := map[string]dbus.Variant{"desktop-entry": dbus.MakeVariant(desktopEntry)}
	call := b.conn.Object(notificationsName, notificationsPath).Go(notificationsIface+".Notify", 0, nil,
		"foxCast", uint32(0), icon, summary, body, []string{}, hints, int32(-1))
	if call.Err != nil {
		log.Printf("notification: %v", call.Err)
	}
}

// trayMenuItem is an entry of the tray icon's context menu.
type trayMenuItem struct {
	label     string
	icon      string
	disabled  bool
	separator bool
	// action runs on the D-Bus goroutine when the entry is clicked.
	action func()
}

// trayItem is the exported StatusNotifierItem and its dbusmenu.
type trayItem struct {
	bus     *sessionBus
	name    string
	props   *prop.Properties
	onClick func(x, y int32, activationToken string)

	mu       sync.Mutex
	menu     []trayMenuItem
	revision uint32
	token    string
}

// sniToolTip is the StatusNotifierItem ToolTip property, (sa(iiay)ss).
type sniToolTip struct {
	IconName string
	Pixmaps  []sniPixmap
	Title    string
	Text     string
}

type sniPixmap struct {
	Width, Height int32
	Data          []byte
}

// newTrayItem registers a tray icon. onClick runs on a D-Bus goroutine with
// the click's global position and, on Wayland, an XDG activation token.
func (b *sessionBus) newTrayItem(icon, tooltip string, onClick func(x, y int32, activationToken string)) (*trayItem, error) {
	var hasWatcher bool
	if err := b.conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, watcherName).Store(&hasWatcher); err != nil {
		return nil, err
	}
	if !hasWatcher {
		return nil, errNoTrayHost
	}

	t := &trayItem{bus: b, name: fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid()), onClick: onClick}
	reply, err := b.conn.RequestName(t.name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return nil, err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return nil, fmt.Errorf("bus name %s is taken", t.name)
	}

	if err := b.conn.Export(sniMethods{t}, sniPath, sniInterface); err != nil {
		return nil, err
	}
	t.props, err = prop.Export(b.conn, sniPath, prop.Map{sniInterface: {
		"Category":          {Value: "ApplicationStatus"},
		"Id":                {Value: "foxcast"},
		"Title":             {Value: "foxCast"},
		"Status":            {Value: "Active"},
		"WindowId":          {Value: int32(0)},
		"IconName":          {Value: icon},
		"IconPixmap":        {Value: []sniPixmap{}},
		"OverlayIconName":   {Value: ""},
		"AttentionIconName": {Value: ""},
		"ToolTip":           {Value: sniToolTip{IconName: icon, Pixmaps: []sniPixmap{}, Title: "foxCast", Text: tooltip}},
		"ItemIsMenu":        {Value: false},
		"Menu":              {Value: dbus.ObjectPath(menuPath)},
	}})
	if err != nil {
		return nil, err
	}
	if err := b.conn.Export(introspect.NewIntrospectable(&introspect.Node{
		Name: sniPath,
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{Name: sniInterface, Methods: introspect.Methods(sniMethods{}), Properties: t.props.Introspection(sniInterface)},
		},
	}), sniPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return nil, err
	}

	if err := b.conn.Export(menuMethods{t}, menuPath, menuInterface); err != nil {
		return nil, err
	}
	menuProps, err := prop.Export(b.conn, menuPath, prop.Map{menuInterface: {
		"Version":       {Value: uint32(3)},
		"TextDirection": {Value: "ltr"},
		"Status":        {Value: "normal"},
		"IconThemePath": {Value: []string{}},
	}})
	if err != nil {
		return nil, err
	}
	if err := b.conn.Export(introspect.NewIntrospectable(&introspect.Node{
		Name: menuPath,
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{Name: menuInterface, Methods: introspect.Methods(menuMethods{}), Properties: menuProps.Introspection(menuInterface)},
		},
	}), menuPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return nil, err
	}

	// Register now, and again whenever the tray host restarts (plasmashell
	// crashing or being replaced).
	if err := b.conn.AddMatchSignal(
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, watcherName),
	); err != nil {
		return nil, err
	}
	signals := make(chan *dbus.Signal, 8)
	b.conn.Signal(signals)
	go func() {
		for s := range signals {
			if s.Name == "org.freedesktop.DBus.NameOwnerChanged" && len(s.Body) == 3 {
				if owner, _ := s.Body[2].(string); owner != "" {
					if err := t.register(); err != nil {
						log.Printf("tray: %v", err)
					}
				}
			}
		}
	}()
	if err := t.register(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *trayItem) register() error {
	return t.bus.conn.Object(watcherName, watcherPath).Call(watcherInterface+".RegisterStatusNotifierItem", 0, t.name).Err
}

// setIcon changes the tray icon and tooltip.
func (t *trayItem) setIcon(icon, tooltip string) {
	t.props.SetMust(sniInterface, "IconName", icon)
	t.props.SetMust(sniInterface, "ToolTip", sniToolTip{IconName: icon, Pixmaps: []sniPixmap{}, Title: "foxCast", Text: tooltip})
	_ = t.bus.conn.Emit(sniPath, sniInterface+".NewIcon")
	_ = t.bus.conn.Emit(sniPath, sniInterface+".NewToolTip")
}

// setMenu replaces the context menu.
func (t *trayItem) setMenu(items []trayMenuItem) {
	t.mu.Lock()
	t.menu = items
	t.revision++
	revision := t.revision
	t.mu.Unlock()
	_ = t.bus.conn.Emit(menuPath, menuInterface+".LayoutUpdated", revision, int32(0))
}

// sniMethods are the StatusNotifierItem methods.
type sniMethods struct{ t *trayItem }

func (m sniMethods) Activate(x, y int32) *dbus.Error {
	m.t.mu.Lock()
	token := m.t.token
	m.t.token = ""
	m.t.mu.Unlock()
	m.t.onClick(x, y, token)
	return nil
}

func (m sniMethods) SecondaryActivate(x, y int32) *dbus.Error { return nil }

// ContextMenu is only called by hosts that do not use the dbusmenu.
func (m sniMethods) ContextMenu(x, y int32) *dbus.Error {
	return m.Activate(x, y)
}

func (m sniMethods) Scroll(delta int32, orientation string) *dbus.Error { return nil }

// ProvideXdgActivationToken receives a Wayland activation token ahead of
// Activate, which lets the popup take focus.
func (m sniMethods) ProvideXdgActivationToken(token string) *dbus.Error {
	m.t.mu.Lock()
	m.t.token = token
	m.t.mu.Unlock()
	return nil
}

// menuLayout is a dbusmenu layout node, (ia{sv}av).
type menuLayout struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

// menuItemProps is a dbusmenu (ia{sv}) property set.
type menuItemProps struct {
	ID    int32
	Props map[string]dbus.Variant
}

// menuEvent is a dbusmenu (isvu) event.
type menuEvent struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}

// menuMethods implement com.canonical.dbusmenu over trayItem.menu. Item i
// has ID i+1; 0 is the root.
type menuMethods struct{ t *trayItem }

func (m menuMethods) snapshot() ([]trayMenuItem, uint32) {
	m.t.mu.Lock()
	defer m.t.mu.Unlock()
	return m.t.menu, m.t.revision
}

func itemProperties(item *trayMenuItem) map[string]dbus.Variant {
	if item.separator {
		return map[string]dbus.Variant{"type": dbus.MakeVariant("separator")}
	}
	props := map[string]dbus.Variant{
		"label":   dbus.MakeVariant(item.label),
		"enabled": dbus.MakeVariant(!item.disabled),
		"visible": dbus.MakeVariant(true),
	}
	if item.icon != "" {
		props["icon-name"] = dbus.MakeVariant(item.icon)
	}
	return props
}

func (m menuMethods) GetLayout(parentID int32, _ int32, _ []string) (uint32, menuLayout, *dbus.Error) {
	items, revision := m.snapshot()
	if parentID != 0 {
		if parentID < 1 || int(parentID) > len(items) {
			return 0, menuLayout{}, dbus.MakeFailedError(fmt.Errorf("no menu item %d", parentID))
		}
		return revision, menuLayout{ID: parentID, Props: itemProperties(&items[parentID-1]), Children: []dbus.Variant{}}, nil
	}
	root := menuLayout{Props: map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}, Children: []dbus.Variant{}}
	for i := range items {
		root.Children = append(root.Children, dbus.MakeVariant(menuLayout{
			ID: int32(i + 1), Props: itemProperties(&items[i]), Children: []dbus.Variant{},
		}))
	}
	return revision, root, nil
}

func (m menuMethods) GetGroupProperties(ids []int32, _ []string) ([]menuItemProps, *dbus.Error) {
	items, _ := m.snapshot()
	out := []menuItemProps{}
	for _, id := range ids {
		if id >= 1 && int(id) <= len(items) {
			out = append(out, menuItemProps{ID: id, Props: itemProperties(&items[id-1])})
		}
	}
	return out, nil
}

func (m menuMethods) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	items, _ := m.snapshot()
	if id < 1 || int(id) > len(items) {
		return dbus.Variant{}, dbus.MakeFailedError(fmt.Errorf("no menu item %d", id))
	}
	v, ok := itemProperties(&items[id-1])[name]
	if !ok {
		return dbus.Variant{}, dbus.MakeFailedError(fmt.Errorf("no property %q", name))
	}
	return v, nil
}

func (m menuMethods) Event(id int32, eventID string, _ dbus.Variant, _ uint32) *dbus.Error {
	if eventID != "clicked" {
		return nil
	}
	items, _ := m.snapshot()
	if id >= 1 && int(id) <= len(items) {
		if item := items[id-1]; item.action != nil && !item.disabled {
			item.action()
		}
	}
	return nil
}

func (m menuMethods) EventGroup(events []menuEvent) ([]int32, *dbus.Error) {
	for _, e := range events {
		_ = m.Event(e.ID, e.EventID, e.Data, e.Timestamp)
	}
	return []int32{}, nil
}

func (m menuMethods) AboutToShow(int32) (bool, *dbus.Error) {
	return false, nil
}

func (m menuMethods) AboutToShowGroup([]int32) ([]int32, []int32, *dbus.Error) {
	return []int32{}, []int32{}, nil
}
