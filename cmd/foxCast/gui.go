package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/gui"
	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

// clockLookupTimeout bounds resolving a manually added receiver's host name
// for its PTP report.
const clockLookupTimeout = time.Second

// cmdGUI runs the system tray front end. The mirror and transmux flags set
// the defaults for sessions started from it.
func cmdGUI(ctx context.Context, args []string) error {
	b := &guiBackend{}
	flags := flag.NewFlagSet("gui", flag.ContinueOnError)
	b.opts.registerSession(flags)
	b.mirror.register(flags)
	b.transmux.register(flags)
	flags.IntVar(&b.httpPort, "http-port", 0, "local TCP port the receiver fetches local/transmuxed media from (0 = random; fix it to open it in a firewall)")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: foxCast gui [flags]\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if err := b.mirror.finish(); err != nil {
		return err
	}
	if err := b.opts.finish(mirrorUDPPorts); err != nil {
		return err
	}
	// One store for all sessions, so a pairing saved by one is seen by the
	// next and "Forget" is not undone by a session's stale copy.
	store, err := newCredentialStore(b.opts.credBackend, b.opts.credFile)
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	b.opts.store = store
	defer b.opts.openPTP()()
	return gui.Run(ctx, b)
}

// guiBackend runs GUI sessions with the same flows as the CLI commands.
type guiBackend struct {
	opts     connectOptions
	mirror   mirrorOptions
	transmux transmuxFlags
	httpPort int
}

func (b *guiBackend) Discover(ctx context.Context) ([]sender.AirPlayDevice, error) {
	return discover(ctx)
}

// Paired counts a saved password as paired too: password receivers such as
// HomePods pair transiently on every connection, so only the password is kept.
func (b *guiBackend) Paired(deviceIDs []string) map[string]bool {
	paired := make(map[string]bool, len(deviceIDs))
	for _, id := range deviceIDs {
		if creds := b.opts.store.Lookup(id); creds.HasPairingCredentials() || (creds != nil && creds.Password != "") {
			paired[id] = true
		}
	}
	return paired
}

func (b *guiBackend) Forget(deviceID string) error {
	return b.opts.store.Forget(deviceID)
}

// connectOptions returns the options for a session with r.
func (b *guiBackend) connectOptions(r *gui.Receiver, prompt gui.Prompter, cb gui.Callbacks) *connectOptions {
	o := b.opts
	if r.Manual {
		o.target, o.port = r.IP, r.Port
	} else {
		device := r.AirPlayDevice
		o.device = &device
	}
	o.prompter = guiPrompter(prompt)
	o.onConnected = func(info *sender.ReceiverInfo) {
		if cb.Connected != nil {
			cb.Connected(info.Name, info.Model, info.DeviceID)
		}
		cb.Status("Pairing…")
	}
	return &o
}

func (b *guiBackend) Mirror(ctx context.Context, r *gui.Receiver, prompt gui.Prompter, cb gui.Callbacks) error {
	cb.Status("Connecting…")
	return runMirror(ctx, b.connectOptions(r, prompt, cb), &b.mirror, mirrorHooks{
		status:     cb.Status,
		started:    cb.Started,
		switchable: cb.SourceSwitchable,
	})
}

func (b *guiBackend) StreamAudio(ctx context.Context, r *gui.Receiver, prompt gui.Prompter, cb gui.Callbacks) error {
	cb.Status("Connecting…")
	mo := b.mirror
	mo.audioOnly, mo.noAudio = true, false
	return runMirror(ctx, b.connectOptions(r, prompt, cb), &mo, mirrorHooks{started: cb.Started})
}

func (b *guiBackend) Play(ctx context.Context, r *gui.Receiver, location string, prompt gui.Prompter, cb gui.Callbacks) error {
	cb.Status("Opening media…")
	m, err := prepareMedia(ctx, location, &b.transmux)
	if err != nil {
		return err
	}
	defer m.close()
	if m.session != nil {
		for _, n := range m.session.Notes() {
			log.Printf("%s: %s", location, n)
		}
	}
	cb.Status("Connecting…")
	return playMedia(ctx, b.connectOptions(r, prompt, cb), location, m, playOptions{
		httpPort:  b.httpPort,
		onStarted: cb.Started,
	})
}

// ClockStats reports the PTP listener's view of r. r.IP is the address the
// session connected to; a manually added host name is looked up again.
func (b *guiBackend) ClockStats(r *gui.Receiver) (gui.ClockStats, bool) {
	if b.opts.ptp == nil {
		return gui.ClockStats{}, false
	}
	addrs := []netip.Addr{}
	if addr, err := netip.ParseAddr(r.IP); err == nil {
		addrs = append(addrs, addr)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), clockLookupTimeout)
		addrs, _ = net.DefaultResolver.LookupNetIP(ctx, "ip4", r.IP)
		cancel()
	}
	for _, addr := range addrs {
		if st, ok := b.opts.ptp.Stats(addr); ok {
			return gui.ClockStats{Locked: st.Locked, Latency: st.Latency, Jitter: st.Jitter, LastSync: st.LastSync}, true
		}
	}
	return gui.ClockStats{}, false
}

// guiPrompter adapts a gui.Prompter to the connect flow.
type guiPrompter gui.Prompter

func (p guiPrompter) promptCredential(ctx context.Context, receiver string, kind credentialKind) (string, error) {
	var k gui.CredentialKind
	switch kind {
	case credentialPassword:
		k = gui.CredentialPassword
	case credentialPIN:
		k = gui.CredentialPIN
	default:
		k = gui.CredentialPINOrPassword
	}
	return p(ctx, receiver, k)
}
