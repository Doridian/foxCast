package main

// Connection and pairing flow shared by all receiver subcommands. Adapted from
// doubletake's cmd/doubletake (LGPL-3.0-or-later).

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

// codeEnvironment carries the pairing PIN / receiver password. It takes
// precedence over -code so the value stays out of shell history and ps.
const codeEnvironment = "FOXCAST_CODE"

// traceEnvironment enables verbose protocol logging, like -debug.
const traceEnvironment = "FOXCAST_TRACE"

const discoveryTimeout = 5 * time.Second

// credentialKind is what a receiver asks the user for, so a prompt can word
// itself (and mask its input) accordingly.
type credentialKind int

const (
	// credentialPassword is the receiver's configured password.
	credentialPassword credentialKind = iota
	// credentialPIN is the one-time PIN the receiver is displaying.
	credentialPIN
	// credentialPINOrPassword is either; the receiver did not say which.
	credentialPINOrPassword
)

// credentialPrompter asks the user for a pairing PIN or receiver password.
// receiver names the receiver asking. An empty result means none was given.
type credentialPrompter interface {
	promptCredential(ctx context.Context, receiver string, kind credentialKind) (string, error)
}

// terminalPrompter prompts on stdin/stdout.
type terminalPrompter struct{}

func (terminalPrompter) promptCredential(_ context.Context, _ string, kind credentialKind) (string, error) {
	switch kind {
	case credentialPassword:
		return readCredential("Enter the receiver's configured password: "), nil
	case credentialPIN:
		return readCredential("Enter the PIN shown on the receiver: "), nil
	default:
		return readCredential("Enter the receiver's configured password or pairing PIN: "), nil
	}
}

// connectOptions are the flags shared by every subcommand that talks to a
// receiver.
type connectOptions struct {
	target      string
	port        int
	code        string
	credFile    string
	credBackend string
	forcePair   bool
	portRange   string
	debug       bool

	portMin, portMax int

	// store, when set, is used instead of opening the -creds/-cred-backend
	// store, so concurrent sessions share one view of it.
	store *sender.CredentialStore
	// device, when set, is the receiver to use instead of discovering one.
	device *sender.AirPlayDevice
	// prompter asks for PINs and passwords; nil prompts on the terminal.
	prompter credentialPrompter
	// onConnected, when set, runs once the receiver has described itself,
	// before pairing.
	onConnected func(*sender.ReceiverInfo)
}

func (o *connectOptions) register(flags *flag.FlagSet) {
	flags.StringVar(&o.target, "target", "", "receiver IP address or hostname (skip discovery)")
	flags.IntVar(&o.port, "port", 7000, "AirPlay port")
	flags.StringVar(&o.code, "code", "", "pairing PIN shown on the receiver, or its configured password; prefer $"+codeEnvironment)
	flags.BoolVar(&o.forcePair, "pair", false, "force new pairing even if credentials exist")
	o.registerSession(flags)
}

// registerSession registers the flags that do not pick or pair a particular
// receiver.
func (o *connectOptions) registerSession(flags *flag.FlagSet) {
	flags.StringVar(&o.credFile, "creds", sender.DefaultCredentialsPath(), "path to saved pairing credentials")
	flags.StringVar(&o.credBackend, "cred-backend", "file", "credential storage backend: file or keyring")
	flags.StringVar(&o.portRange, "port-range", "", "local UDP port range for timing/audio (e.g. 60000-60010); empty = ephemeral")
	flags.BoolVar(&o.debug, "debug", false, "verbose protocol logging")
}

// finish validates the parsed flags and applies process-wide settings.
func (o *connectOptions) finish(minPorts int) error {
	var err error
	o.portMin, o.portMax, err = parsePortRange(o.portRange, minPorts)
	if err != nil {
		return fmt.Errorf("invalid -port-range: %w", err)
	}
	if env := os.Getenv(codeEnvironment); env != "" {
		o.code = env
	}
	sender.SetDebugMode(o.debug || os.Getenv(traceEnvironment) != "")
	return nil
}

// askCredential prompts for a credential, treating an empty answer as an
// error named by what.
func (o *connectOptions) askCredential(ctx context.Context, receiver string, kind credentialKind, what string) (string, error) {
	p := o.prompter
	if p == nil {
		p = terminalPrompter{}
	}
	value, err := p.promptCredential(ctx, receiver, kind)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("%s cannot be empty", what)
	}
	return value, nil
}

// connection is a paired, encrypted control connection to a receiver.
type connection struct {
	// addr is the receiver's control address ("host:port").
	addr   string
	client *sender.AirPlayClient
	info   *sender.ReceiverInfo
	store  *sender.CredentialStore
}

// connect selects a receiver, connects, and pairs (pair-verify with stored
// credentials, transient pairing, or PIN pairing as the receiver requires).
// When fairPlay is set, the FairPlay SAP handshake is run afterwards.
func connect(ctx context.Context, o *connectOptions, fairPlay bool) (*connection, error) {
	addr, port := o.target, o.port
	advertisement := o.device
	if advertisement != nil {
		addr, port = advertisement.IP, advertisement.Port
	} else if addr == "" {
		device, err := selectDevice(ctx)
		if err != nil {
			return nil, fmt.Errorf("discovery: %w", err)
		}
		addr, port, advertisement = device.IP, device.Port, device
		fmt.Printf("selected: %s (%s:%d)\n", device.Name, device.IP, device.Port)
	}

	store := o.store
	if store == nil {
		var err error
		if store, err = newCredentialStore(o.credBackend, o.credFile); err != nil {
			return nil, fmt.Errorf("load credentials: %w", err)
		}
	}

	// A receiver's configured password is saved alongside its pairing, so it
	// is only prompted for once. -code overrides it; -pair ignores it so a
	// changed password can be re-entered.
	var saved *sender.SavedCredentials
	credential := o.code
	newClient := func() *sender.AirPlayClient {
		if advertisement != nil {
			device := *advertisement
			device.IP, device.Port = addr, port
			return sender.NewAirPlayClientForDevice(device)
		}
		return sender.NewAirPlayClient(addr, port)
	}
	c := &connection{addr: net.JoinHostPort(addr, strconv.Itoa(port)), client: newClient(), store: store}
	open := func() error {
		c.client.SetPassword(credential)
		if err := c.client.Connect(ctx); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		info, err := c.client.GetInfo()
		if err != nil {
			return fmt.Errorf("get info: %w", err)
		}
		c.info = info
		return nil
	}
	reconnect := func() error {
		_ = c.client.Close()
		c.client = newClient()
		return open()
	}
	if err := open(); err != nil {
		_ = c.client.Close()
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = c.client.Close()
		}
	}()
	log.Printf("connected to: %s (model: %s)", c.info.Name, c.info.Model)
	if o.onConnected != nil {
		o.onConnected(c.info)
	}

	if !o.forcePair {
		saved = store.Lookup(c.info.DeviceID)
	}
	if credential == "" && saved != nil && saved.Password != "" {
		log.Printf("using saved receiver password (%s)", o.credBackend)
		credential = saved.Password
		c.client.SetPassword(credential)
	}
	if credential == "" && c.info.RequiresPassword() {
		var err error
		credential, err = o.askCredential(ctx, c.info.Name, credentialPassword, "password")
		if err != nil {
			return nil, err
		}
		c.client.SetPassword(credential)
	}

	savePairing := func() {
		if c.client.PairKeys == nil {
			return
		}
		if err := store.SavePairing(c.info.DeviceID, c.client.PairingID,
			c.client.PairKeys.Ed25519Public, c.client.PairKeys.Ed25519Private,
			c.client.PairingProtocol()); err != nil {
			log.Printf("warning: failed to save credentials: %v", err)
		} else {
			log.Printf("credentials saved (%s)", o.credBackend)
		}
	}
	pairWithCredential := func(expectPIN bool) error {
		value := credential
		if value == "" {
			if err := c.client.StartPINDisplay(); err != nil {
				log.Printf("warning: failed to trigger PIN display: %v", err)
				expectPIN = false
			}
			kind := credentialPINOrPassword
			if expectPIN {
				kind = credentialPIN
			}
			var err error
			value, err = o.askCredential(ctx, c.info.Name, kind, "pairing credential")
			if err != nil {
				return err
			}
		}
		// The same user-entered value may be needed for Digest auth later.
		credential = value
		if err := c.client.Pair(ctx, value); err != nil {
			return fmt.Errorf("pairing: %w", err)
		}
		savePairing()
		return nil
	}
	// pairTransientCodes runs transient pairing with each candidate setup code,
	// on a fresh connection per retry. Only an SRP authentication failure moves
	// on to the next code; anything else is reported immediately.
	pairTransientCodes := func() error {
		codes := sender.TransientSetupCodes(credential)
		var err error
		for i, code := range codes {
			if i > 0 {
				if err := reconnect(); err != nil {
					return err
				}
			}
			err = c.client.PairTransientWithCode(ctx, code)
			if !errors.Is(err, sender.ErrPairingAuthentication) {
				return err
			}
			log.Printf("transient pair-setup code %d/%d rejected", i+1, len(codes))
		}
		return err
	}
	// pairFresh pairs without stored credentials: SRP when the receiver asks
	// for a PIN/password, otherwise transient pairing with a PIN fallback.
	pairFresh := func() error {
		switch c.info.RequiredPairingCredential() {
		case sender.PairingCredentialPassword:
			return pairWithCredential(false)
		case sender.PairingCredentialPIN:
			return pairWithCredential(true)
		}
		if c.info.RequiresPassword() {
			if err := pairTransientCodes(); err != nil {
				return fmt.Errorf("transient pairing failed for password-protected receiver: %w", err)
			}
			return nil
		}
		err := c.client.Pair(ctx, "")
		if err == nil {
			return nil
		}
		log.Printf("transient pairing failed: %v, requesting pairing credentials", err)
		// A failed exchange may leave receiver state on this socket.
		if err := reconnect(); err != nil {
			return err
		}
		return pairWithCredential(false)
	}

	switch {
	case o.forcePair && c.info.RequiresPassword() && c.info.RequiredPairingCredential() == sender.PairingCredentialNone:
		// A modern receiver's playback password belongs only to HTTP Digest;
		// SRP rejects it as a bad PIN.
		if err := pairTransientCodes(); err != nil {
			return nil, fmt.Errorf("transient pairing failed for password-protected receiver: %w", err)
		}
	case o.forcePair:
		if err := pairWithCredential(!c.info.RequiresPassword()); err != nil {
			return nil, err
		}
	case saved != nil && saved.HasPairingCredentials():
		log.Printf("using saved credentials (%s)", o.credBackend)
		verifyErr := c.client.RestorePairingCredentials(saved)
		if verifyErr == nil {
			verifyErr = c.client.PairVerify(ctx)
		}
		if verifyErr != nil {
			log.Printf("pair-verify with saved credentials failed: %v", verifyErr)
			if err := reconnect(); err != nil {
				return nil, err
			}
			if err := pairFresh(); err != nil {
				return nil, err
			}
		}
	default:
		if err := pairFresh(); err != nil {
			return nil, err
		}
	}
	log.Println("pairing complete")
	// Only a configured password is worth keeping; a one-time PIN is not.
	if c.info.RequiresPassword() && (saved == nil || saved.Password != credential) {
		c.savePassword(credential)
	}

	if fairPlay && c.client.FpEkey == nil {
		if err := c.client.FairPlaySetup(ctx); err != nil {
			if !errors.Is(err, sender.ErrFairPlayUnsupported) {
				return nil, fmt.Errorf("FairPlay setup: %w", err)
			}
			log.Printf("FairPlay SAP unsupported (%v); continuing", err)
		} else {
			log.Println("FairPlay setup complete")
		}
	}

	ok = true
	return c, nil
}

// savePassword stores the receiver's configured password so later launches
// need not prompt for it.
func (c *connection) savePassword(password string) {
	if password == "" {
		return
	}
	if err := c.store.SavePassword(c.info.DeviceID, password); err != nil {
		log.Printf("warning: failed to save receiver password: %v", err)
	} else {
		log.Println("receiver password saved")
	}
}

func (c *connection) Close() error {
	return c.client.Close()
}

// parsePortRange parses "min-max" into inclusive bounds. An empty string
// returns (0, 0) meaning "let the OS pick".
func parsePortRange(s string, minPorts int) (int, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("expected MIN-MAX, got %q", s)
	}
	min, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil {
		return 0, 0, fmt.Errorf("min: %w", err)
	}
	max, err := strconv.Atoi(strings.TrimSpace(hi))
	if err != nil {
		return 0, 0, fmt.Errorf("max: %w", err)
	}
	if min < 1 || max > 65535 || min > max {
		return 0, 0, fmt.Errorf("range %d-%d out of bounds (1-65535, min<=max)", min, max)
	}
	if max-min+1 < minPorts {
		return 0, 0, fmt.Errorf("range %d-%d too small; need %d consecutive UDP ports", min, max, minPorts)
	}
	return min, max, nil
}

func readCredential(prompt string) string {
	fmt.Print(prompt)
	// Read the whole line: a password may contain spaces.
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

func discover(ctx context.Context) ([]sender.AirPlayDevice, error) {
	discoverCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	devices, err := sender.DiscoverAirPlayDevices(discoverCtx)
	if err != nil {
		return nil, err
	}
	sort.Slice(devices, func(i, j int) bool {
		return compareIPs(devices[i].IP, devices[j].IP) < 0
	})
	return devices, nil
}

func selectDevice(ctx context.Context) (*sender.AirPlayDevice, error) {
	fmt.Println("searching for AirPlay receivers...")
	devices, err := discover(ctx)
	if err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		return nil, errors.New("no AirPlay receivers found")
	}
	fmt.Println("\navailable devices:")
	for i, d := range devices {
		fmt.Printf("  [%d] %s (%s) - %s\n", i+1, d.Name, d.Model, d.IP)
	}
	input := strings.TrimSpace(readCredential("\nselect device [1]: "))
	if input == "" {
		return &devices[0], nil
	}
	idx, err := strconv.Atoi(input)
	if err != nil || idx < 1 || idx > len(devices) {
		return nil, errors.New("invalid selection")
	}
	return &devices[idx-1], nil
}

// compareIPs orders IP address strings numerically, non-IPs last.
func compareIPs(a, b string) int {
	ipA, ipB := net.ParseIP(a), net.ParseIP(b)
	switch {
	case ipA == nil && ipB == nil:
		return strings.Compare(a, b)
	case ipA == nil:
		return 1
	case ipB == nil:
		return -1
	}
	return strings.Compare(string(ipA.To16()), string(ipB.To16()))
}

func newCredentialStore(backend, filePath string) (*sender.CredentialStore, error) {
	switch backend {
	case "keyring":
		kb, err := sender.NewKeyringBackend()
		if err != nil {
			return nil, err
		}
		return sender.NewCredentialStoreWithBackend(kb), nil
	case "file":
		return sender.NewCredentialStore(filePath)
	default:
		return nil, fmt.Errorf("unknown credential backend %q (use \"file\" or \"keyring\")", backend)
	}
}
