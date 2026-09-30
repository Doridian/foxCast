package main

// Opening links in an Apple TV's own apps (YouTube and others) over the
// Companion protocol, instead of playing them through AirPlay.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/applink"
	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

// companionLookupTimeout bounds browsing for a receiver's Companion service.
const companionLookupTimeout = 5 * time.Second

// companionClientName is how the Apple TV lists foxCast among its remotes.
const companionClientName = "foxCast"

// appMode is play's -app setting.
type appMode string

const (
	// appAuto opens links of sites with a known Apple TV app in that app.
	appAuto appMode = "auto"
	// appAlways hands every URL to the Apple TV to open, rewritten for a
	// known app when there is one.
	appAlways appMode = "always"
	// appNever plays everything through AirPlay.
	appNever appMode = "never"
)

func parseAppMode(s string) (appMode, error) {
	switch m := appMode(s); m {
	case appAuto, appAlways, appNever:
		return m, nil
	}
	return "", fmt.Errorf("invalid -app %q (want auto, always or never)", s)
}

// appLink returns the link to open in a receiver app instead of playing
// location through AirPlay.
func appLink(location string, mode appMode) (applink.Link, bool) {
	if mode == appNever {
		return applink.Link{}, false
	}
	if link, ok := applink.Resolve(location); ok {
		return link, true
	}
	if mode != appAlways {
		return applink.Link{}, false
	}
	if _, err := os.Stat(location); err == nil {
		return applink.Link{}, false
	}
	if u, err := url.Parse(location); err == nil && u.Scheme != "" {
		return applink.Link{App: "the app that handles it", URL: location}, true
	}
	return applink.Link{}, false
}

// openInApp opens link on the receiver o selects, pairing with its
// Companion service first if needed.
func openInApp(ctx context.Context, o *connectOptions, link applink.Link, status func(string)) error {
	client, creds, err := companionConnectReceiver(ctx, o, status)
	if err != nil {
		return fmt.Errorf("open in %s: %w", link.App, err)
	}
	defer client.Close()

	if err := client.StartSession(ctx, sender.CompanionSystemInfo{Name: companionClientName, PairingID: creds.PairingID}); err != nil {
		return err
	}
	log.Printf("opening %s", link.URL)
	if err := client.LaunchApp(ctx, link.URL); err != nil {
		return fmt.Errorf("open in %s: %w", link.App, err)
	}
	return nil
}

// companionConnectReceiver finds the Companion service of the receiver o
// selects and returns a verified connection to it.
func companionConnectReceiver(ctx context.Context, o *connectOptions, status func(string)) (*sender.CompanionClient, *sender.CompanionCredentials, error) {
	if status == nil {
		status = func(string) {}
	}
	r, err := companionReceiver(ctx, o)
	if err != nil {
		return nil, nil, err
	}

	port := o.companionPort
	if port == 0 {
		status("Looking for the Apple TV's remote service…")
		lookupCtx, cancel := context.WithTimeout(ctx, companionLookupTimeout)
		service, err := sender.FindCompanionService(lookupCtx, r.host)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			return nil, nil, err
		}
		if service.PairingDisabled() {
			log.Printf("warning: %s advertises Companion pairing as disabled", service.Name)
		}
		port = service.Port
	}
	addr := net.JoinHostPort(r.host, strconv.Itoa(port))

	store := o.store
	if store == nil {
		if store, err = newCredentialStore(o.credBackend, o.credFile); err != nil {
			return nil, nil, fmt.Errorf("load credentials: %w", err)
		}
	}

	status("Connecting…")
	return companionConnect(ctx, o, store, addr, r)
}

// appReceiver identifies the receiver to open an app link on.
type appReceiver struct {
	name string
	// host is its IP address.
	host string
	// deviceID is its AirPlay device ID, the key of its saved credentials.
	deviceID string
}

// companionReceiver returns the receiver o selects.
func companionReceiver(ctx context.Context, o *connectOptions) (appReceiver, error) {
	if o.device != nil {
		return appReceiver{o.device.Name, o.device.IP, o.device.DeviceID}, nil
	}
	if o.target == "" {
		device, err := selectDevice(ctx)
		if err != nil {
			return appReceiver{}, fmt.Errorf("discovery: %w", err)
		}
		fmt.Printf("selected: %s (%s)\n", device.Name, device.IP)
		return appReceiver{device.Name, device.IP, device.DeviceID}, nil
	}

	// Credentials are keyed by the AirPlay device ID, which /info reports
	// without pairing.
	client := sender.NewAirPlayClient(o.target, o.port)
	if err := client.Connect(ctx); err != nil {
		return appReceiver{}, fmt.Errorf("connect: %w", err)
	}
	info, err := client.GetInfo()
	_ = client.Close()
	if err != nil {
		return appReceiver{}, fmt.Errorf("get info: %w", err)
	}
	if o.onConnected != nil {
		o.onConnected(info)
	}
	host := o.target
	if net.ParseIP(host) == nil {
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			return appReceiver{}, fmt.Errorf("resolve %s: %w", host, err)
		}
		host = addrs[0]
	}
	return appReceiver{info.Name, host, info.DeviceID}, nil
}

// companionConnect returns a verified Companion connection: with saved
// credentials, or after PIN pairing (whose credentials it saves).
func companionConnect(ctx context.Context, o *connectOptions, store *sender.CredentialStore, addr string, r appReceiver) (*sender.CompanionClient, *sender.CompanionCredentials, error) {
	var saved *sender.CompanionCredentials
	if !o.forcePair {
		if creds := store.Lookup(r.deviceID); creds != nil {
			saved = creds.Companion
		}
	}
	if saved.Valid() {
		client, err := sender.DialCompanion(ctx, addr)
		if err != nil {
			return nil, nil, err
		}
		err = client.Verify(ctx, saved)
		if err == nil {
			log.Printf("using saved Companion credentials (%s)", o.credBackend)
			return client, saved, nil
		}
		_ = client.Close()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		log.Printf("Companion pair-verify with saved credentials failed: %v", err)
	}

	creds, err := companionPair(ctx, o, addr, r.name)
	if err != nil {
		return nil, nil, err
	}
	if err := store.SaveCompanion(r.deviceID, creds); err != nil {
		log.Printf("warning: failed to save Companion credentials: %v", err)
	} else {
		log.Printf("Companion credentials saved (%s)", o.credBackend)
	}

	// pyatv verifies on a new connection after pairing, as the Remote app does.
	client, err := sender.DialCompanion(ctx, addr)
	if err != nil {
		return nil, nil, err
	}
	if err := client.Verify(ctx, creds); err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("Companion pair-verify after pairing: %w", err)
	}
	return client, creds, nil
}

// companionPair runs PIN pair-setup: the Apple TV shows a PIN for the user
// to enter.
func companionPair(ctx context.Context, o *connectOptions, addr, receiver string) (*sender.CompanionCredentials, error) {
	client, err := sender.DialCompanion(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	pairing, err := client.BeginPairing(ctx)
	if err != nil {
		return nil, fmt.Errorf("Companion pairing: %w", err)
	}
	pin, err := o.askCredential(ctx, receiver, credentialPIN, "PIN")
	if err != nil {
		return nil, err
	}
	creds, err := pairing.Finish(ctx, pin, companionClientName)
	if errors.Is(err, sender.ErrPairingAuthentication) {
		return nil, errors.New("Companion pairing: the Apple TV rejected the PIN")
	}
	if err != nil {
		return nil, fmt.Errorf("Companion pairing: %w", err)
	}
	return creds, nil
}
