// Package gui is foxCast's system tray front end. The Qt user interface is
// only built with the "gui" build tag (it needs CGo and the Qt 6 development
// files); without it, Run reports that the GUI is unavailable. The receiver
// list and input handling in this file are toolkit-independent.
package gui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

const (
	// defaultAirPlayPort is used for manually added receivers without a port.
	defaultAirPlayPort = 7000
	// receiverExpiry is how long a receiver stays listed after it stops
	// answering discovery; mDNS responses are occasionally missed.
	receiverExpiry = 90 * time.Second
	// clockStaleAfter is how long without a PTP Sync before the clock is
	// shown as lost.
	clockStaleAfter = 3 * time.Second
)

// ErrCancelled is returned by a Prompter when the user dismisses a prompt.
var ErrCancelled = errors.New("cancelled by user")

// ErrUnavailable is returned by Run when foxCast was built without the GUI.
var ErrUnavailable = errors.New("foxCast was built with -tags nogui; rebuild without it for the tray app (needs Qt 6 development files)")

// CredentialKind is what a receiver asks the user for.
type CredentialKind int

const (
	// CredentialPassword is the receiver's configured password.
	CredentialPassword CredentialKind = iota
	// CredentialPIN is the one-time PIN the receiver is displaying.
	CredentialPIN
	// CredentialPINOrPassword is either; the receiver did not say which.
	CredentialPINOrPassword
)

// Prompter asks the user for a pairing PIN or receiver password. It returns
// ErrCancelled when the user declines.
type Prompter func(ctx context.Context, receiver string, kind CredentialKind) (string, error)

// Receiver is a receiver the GUI can cast to.
type Receiver struct {
	sender.AirPlayDevice
	// Manual is set for receivers added by address rather than discovered;
	// their advertisement fields are empty.
	Manual bool
}

// Key identifies the receiver across discovery rounds.
func (r *Receiver) Key() string {
	if r.DeviceID != "" && !r.Manual {
		return r.DeviceID
	}
	return net.JoinHostPort(r.IP, strconv.Itoa(r.Port))
}

// CanMirror reports whether the receiver accepts screen mirroring. Manually
// added receivers are unknown, so they are given the benefit of the doubt.
func (r *Receiver) CanMirror() bool {
	return r.Manual || r.SupportsScreen()
}

// CanPlay reports whether the receiver accepts video URL playback.
func (r *Receiver) CanPlay() bool {
	return r.Manual || r.SupportsVideo()
}

// CanStreamAudio reports whether the receiver can be used as a speaker for
// the computer's sound.
func (r *Receiver) CanStreamAudio() bool {
	return r.Manual || r.SupportsAudio()
}

// Usable reports whether foxCast can send anything to the receiver.
func (r *Receiver) Usable() bool {
	return r.CanMirror() || r.CanPlay() || r.CanStreamAudio()
}

// Callbacks runs a session's progress notifications.
type Callbacks struct {
	// Status reports progress ("Connecting", "Pairing", ...).
	Status func(string)
	// Connected reports how the receiver described itself.
	Connected func(name, model, deviceID string)
	// Started runs once media or frames reach the receiver.
	Started func()
	// OpenedInApp runs instead of Started when the location was handed to
	// one of the receiver's apps; the session then ends on its own.
	OpenedInApp func(app string)
	// SourceSwitchable hands over a function that asks the user for a new
	// screen or window and switches a running mirror session to it. It is
	// only called when the capture can offer that choice.
	SourceSwitchable func(switchSource func(context.Context) error)
}

// ClockStats describes the PTP timing of a session.
type ClockStats struct {
	// Locked is set once PTP times the session.
	Locked bool
	// Latency is the typical round trip to the receiver.
	Latency time.Duration
	// Jitter is how much the round trip varies.
	Jitter time.Duration
	// LastSync is when the receiver's clock was last heard.
	LastSync time.Time
}

// clockStatsText is the short PTP line shown under a connected receiver.
func clockStatsText(st ClockStats, now time.Time) string {
	switch {
	case !st.Locked:
		return "PTP · synchronizing…"
	case now.Sub(st.LastSync) > clockStaleAfter:
		return "PTP · no sync from receiver"
	default:
		return fmt.Sprintf("PTP · %s latency · %s jitter", milliseconds(st.Latency), milliseconds(st.Jitter))
	}
}

// milliseconds formats d like "4.2 ms".
func milliseconds(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64) + " ms"
}

// Backend performs the receiver work the GUI triggers. Mirror and Play block
// until the session ends; cancelling ctx stops the session and is not an
// error.
type Backend interface {
	Discover(ctx context.Context) ([]sender.AirPlayDevice, error)
	// Paired reports which of the device IDs have a saved pairing or password.
	Paired(deviceIDs []string) map[string]bool
	// Forget deletes everything saved for a device.
	Forget(deviceID string) error
	Mirror(ctx context.Context, r *Receiver, prompt Prompter, cb Callbacks) error
	// StreamAudio sends the computer's sound, and no video, to r.
	StreamAudio(ctx context.Context, r *Receiver, prompt Prompter, cb Callbacks) error
	Play(ctx context.Context, r *Receiver, location string, prompt Prompter, cb Callbacks) error
	// ClockStats reports on the PTP clock of r's running session; ok is false
	// when the session is not timed with PTP (or not yet).
	ClockStats(r *Receiver) (stats ClockStats, ok bool)
}

// receiverList merges discovery rounds, keeping receivers for a while after
// they stop answering.
type receiverList struct {
	entries map[string]*listEntry
}

type listEntry struct {
	receiver Receiver
	lastSeen time.Time
}

func newReceiverList() *receiverList {
	return &receiverList{entries: map[string]*listEntry{}}
}

// update records a discovery round at now and reports whether the listing
// changed.
func (l *receiverList) update(devices []sender.AirPlayDevice, now time.Time) bool {
	changed := false
	for _, d := range devices {
		r := Receiver{AirPlayDevice: d}
		key := r.Key()
		e, ok := l.entries[key]
		if !ok || !sameListing(&e.receiver, &r) {
			changed = true
		}
		l.entries[key] = &listEntry{receiver: r, lastSeen: now}
	}
	for key, e := range l.entries {
		if !e.receiver.Manual && now.Sub(e.lastSeen) > receiverExpiry {
			delete(l.entries, key)
			changed = true
		}
	}
	return changed
}

// sameListing reports whether a and b would be shown identically.
func sameListing(a, b *Receiver) bool {
	return a.Name == b.Name && a.Model == b.Model && a.IP == b.IP && a.Port == b.Port &&
		a.CanMirror() == b.CanMirror() && a.CanPlay() == b.CanPlay() && a.CanStreamAudio() == b.CanStreamAudio()
}

// addManual adds a receiver by address and returns it.
func (l *receiverList) addManual(host string, port int) Receiver {
	r := Receiver{AirPlayDevice: sender.AirPlayDevice{Name: host, IP: host, Port: port}, Manual: true}
	l.entries[r.Key()] = &listEntry{receiver: r}
	return r
}

// identify records how a receiver described itself on connecting. Only
// manually added receivers take it; discovered ones keep their
// advertisements. It reports whether the listing changed.
func (l *receiverList) identify(key, name, model, deviceID string) bool {
	e, ok := l.entries[key]
	if !ok || !e.receiver.Manual || name == "" {
		return false
	}
	r := &e.receiver
	if r.Name == name && r.Model == model && r.DeviceID == deviceID {
		return false
	}
	r.Name, r.Model, r.DeviceID = name, model, deviceID
	return true
}

// deviceIDs returns the known device IDs of the listed receivers.
func (l *receiverList) deviceIDs() []string {
	var ids []string
	for _, e := range l.entries {
		if e.receiver.DeviceID != "" {
			ids = append(ids, e.receiver.DeviceID)
		}
	}
	sort.Strings(ids)
	return ids
}

// groupReceivers splits rs (already sorted) for display, like connected,
// known and available networks: receivers that are in use, then paired
// ones, then the rest, with ones foxCast cannot use last. Only receivers
// matching query are kept.
func groupReceivers(rs []Receiver, paired, inUse func(*Receiver) bool, query string) (active, known, other []Receiver) {
	var unusable []Receiver
	for i := range rs {
		r := &rs[i]
		switch {
		case !matchesQuery(r, query):
		case inUse(r):
			active = append(active, *r)
		case paired(r):
			known = append(known, *r)
		case r.Usable():
			other = append(other, *r)
		default:
			unusable = append(unusable, *r)
		}
	}
	return active, known, append(other, unusable...)
}

// matchesQuery reports whether r's name, model or address contains query,
// ignoring case.
func matchesQuery(r *Receiver, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	for _, field := range []string{r.Name, r.Model, r.IP} {
		if strings.Contains(strings.ToLower(field), query) {
			return true
		}
	}
	return false
}

// Layer-shell anchor bits (zwlr_layer_surface_v1.anchor).
const (
	anchorTop    = 1
	anchorBottom = 2
	anchorLeft   = 4
	anchorRight  = 8
)

// rect is a screen-space rectangle.
type rect struct{ x, y, w, h int }

// placement positions the popup: as layer-shell anchors and left/top
// margins relative to the screen, and as a global position for window
// systems that allow placing windows directly.
type placement struct {
	anchors   int
	left, top int
	x, y      int
}

// popupPlacement places a w×h popup opened by a click at (cx, cy) on a
// screen whose usable area is screen. Like a Plasma applet popup, it hugs
// the screen edge nearest the click (where the panel is), centred on the
// click along that edge and kept on screen.
func popupPlacement(cx, cy int, screen rect, w, h int) placement {
	clamp := func(v, lo, hi int) int {
		if v > hi {
			v = hi
		}
		if v < lo {
			v = lo
		}
		return v
	}
	cx = clamp(cx, screen.x, screen.x+screen.w)
	cy = clamp(cy, screen.y, screen.y+screen.h)
	toLeft, toRight := cx-screen.x, screen.x+screen.w-cx
	toTop, toBottom := cy-screen.y, screen.y+screen.h-cy
	along := clamp(cx-screen.x-w/2, 0, max(screen.w-w, 0))
	across := clamp(cy-screen.y-h/2, 0, max(screen.h-h, 0))

	var p placement
	switch min(toLeft, toRight, toTop, toBottom) {
	case toBottom:
		p = placement{anchors: anchorBottom | anchorLeft, left: along}
		p.x, p.y = screen.x+along, screen.y+screen.h-h
	case toTop:
		p = placement{anchors: anchorTop | anchorLeft, left: along}
		p.x, p.y = screen.x+along, screen.y
	case toLeft:
		p = placement{anchors: anchorLeft | anchorTop, top: across}
		p.x, p.y = screen.x, screen.y+across
	default:
		p = placement{anchors: anchorRight | anchorTop, top: across}
		p.x, p.y = screen.x+screen.w-w, screen.y+across
	}
	return p
}

// receivers returns the listing sorted by name.
func (l *receiverList) receivers() []Receiver {
	out := make([]Receiver, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e.receiver)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].Key() < out[j].Key()
	})
	return out
}

// parseAddress parses "host", "host:port" or "[v6]:port".
func parseAddress(s string) (string, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0, errors.New("enter a host name or IP address")
	}
	host, portText, err := net.SplitHostPort(s)
	if err != nil {
		// No port (or a bare IPv6 address).
		return strings.Trim(s, "[]"), defaultAirPlayPort, nil
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", portText)
	}
	if host == "" {
		return "", 0, errors.New("missing host")
	}
	return host, port, nil
}

// mediaLocation interprets text (typed, pasted or dropped) as something to
// play: a URL or an existing local file. It returns "" when text is neither.
func mediaLocation(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, "\n\r") {
		return ""
	}
	if u, err := url.Parse(text); err == nil {
		switch {
		case strings.EqualFold(u.Scheme, "file"):
			return existingFile(u.Path)
		case u.Scheme != "" && u.Host != "":
			return text
		}
	}
	if strings.HasPrefix(text, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			text = filepath.Join(home, text[2:])
		}
	}
	return existingFile(text)
}

func existingFile(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	if info, err := os.Stat(p); err != nil || info.IsDir() {
		return ""
	}
	return p
}

// mediaTitle is a short name for a media location, for status text.
func mediaTitle(location string) string {
	if u, err := url.Parse(location); err == nil && u.Scheme != "" && u.Host != "" {
		if name := path.Base(u.Path); name != "." && name != "/" {
			if unescaped, err := url.PathUnescape(name); err == nil {
				return unescaped
			}
			return name
		}
		return u.Host
	}
	return filepath.Base(location)
}
