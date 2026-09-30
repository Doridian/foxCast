package sender

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// SavedCredentials holds the persistent pairing credentials, the receiver's
// configured password, and optional screencast restore state for a single
// device.
type SavedCredentials struct {
	PairingID       string          `json:"pairing_id"`
	Ed25519Public   []byte          `json:"ed25519_public"`
	Ed25519Seed     []byte          `json:"ed25519_seed"` // 32-byte seed (private key is derived from this)
	PairingProtocol PairingProtocol `json:"pairing_protocol,omitempty"`
	// Password is the receiver's configured playback password ("Require
	// Password"), used for HTTP Digest and password-keyed transient pairing.
	// One-time on-screen PINs are never stored.
	Password     string `json:"password,omitempty"`
	RestoreToken string `json:"restore_token,omitempty"`
	// Companion is the Apple TV's separate Companion (remote control)
	// pairing, used to open links in its apps.
	Companion *CompanionCredentials `json:"companion,omitempty"`
}

// PairingProtocol is the pair-verify wire protocol negotiated when credentials
// were created. It must be restored before reusing the keys: HAP pair-verify
// enables encrypted control framing, while raw AirPlay pair-verify does not.
type PairingProtocol string

const (
	PairingProtocolUnknown PairingProtocol = ""
	PairingProtocolHAP     PairingProtocol = "hap"
	PairingProtocolRaw     PairingProtocol = "raw"
)

// DefaultCredentialsPath returns ~/.config/foxcast/credentials.json.
func DefaultCredentialsPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "foxcast", "credentials.json")
}

// Ed25519Keys reconstructs the key pair from saved credentials.
func (c *SavedCredentials) Ed25519Keys() (ed25519.PublicKey, ed25519.PrivateKey) {
	if len(c.Ed25519Seed) != ed25519.SeedSize {
		return nil, nil
	}
	priv := ed25519.NewKeyFromSeed(c.Ed25519Seed)
	return ed25519.PublicKey(c.Ed25519Public), priv
}

// HasPairingCredentials reports whether the saved entry contains a usable
// AirPlay pairing identity.
func (c *SavedCredentials) HasPairingCredentials() bool {
	return c != nil && c.PairingID != "" &&
		len(c.Ed25519Public) == ed25519.PublicKeySize &&
		len(c.Ed25519Seed) == ed25519.SeedSize
}

// RestorePairingCredentials installs a saved identity and its negotiated wire
// protocol on the client. Call this before PairVerify.
func (c *AirPlayClient) RestorePairingCredentials(saved *SavedCredentials) error {
	if saved == nil || !saved.HasPairingCredentials() {
		return fmt.Errorf("saved credentials contain no usable pairing identity")
	}
	protocol := saved.PairingProtocol
	if protocol == PairingProtocolUnknown {
		// Old credential files predate explicit protocol persistence. Preserve the
		// previous capability-driven behavior: legacy pairing profiles used raw
		// pair-verify, while CoreUtils/HAP profiles used encrypted verification.
		protocol = PairingProtocolHAP
		if c.info != nil && c.info.PrefersLegacyPairing() {
			protocol = PairingProtocolRaw
		}
	}
	switch protocol {
	case PairingProtocolHAP:
		c.pairingProtocol = pairingProtocolHAP
	case PairingProtocolRaw:
		c.pairingProtocol = pairingProtocolRaw
	default:
		return fmt.Errorf("saved credentials contain unknown pairing protocol %q", protocol)
	}

	pub, priv := saved.Ed25519Keys()
	c.PairingID = saved.PairingID
	c.PairKeys = &PairKeys{
		Ed25519Public:  append(ed25519.PublicKey(nil), pub...),
		Ed25519Private: append(ed25519.PrivateKey(nil), priv...),
	}
	return nil
}

// CredentialBackend is the storage interface for pairing credentials.
type CredentialBackend interface {
	Lookup(deviceID string) (*SavedCredentials, error)
	Save(deviceID string, creds *SavedCredentials) error
	// Delete removes a device's credentials; deleting a missing device is
	// not an error.
	Delete(deviceID string) error
}

// CredentialStore manages per-device pairing credentials using a pluggable backend.
type CredentialStore struct {
	mu      sync.Mutex
	backend CredentialBackend
}

// NewCredentialStore creates a credential store backed by a JSON file at path.
func NewCredentialStore(path string) (*CredentialStore, error) {
	fb, err := newFileBackend(path)
	if err != nil {
		return nil, err
	}
	return &CredentialStore{backend: fb}, nil
}

// NewCredentialStoreWithBackend creates a credential store with the given backend.
func NewCredentialStoreWithBackend(b CredentialBackend) *CredentialStore {
	return &CredentialStore{backend: b}
}

// Lookup returns saved credentials for a device, or nil if not found.
func (cs *CredentialStore) Lookup(deviceID string) *SavedCredentials {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	creds, err := cs.backend.Lookup(deviceID)
	if err != nil {
		return nil
	}
	return creds
}

// Len returns the number of stored credential entries.
// Only supported by the file backend; returns 0 for other backends.
func (cs *CredentialStore) Len() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if fb, ok := cs.backend.(*fileBackend); ok {
		return len(fb.devices)
	}
	return 0
}

// Save stores credentials for a device.
func (cs *CredentialStore) Save(deviceID string, pairingID string, pub ed25519.PublicKey, priv ed25519.PrivateKey) error {
	return cs.savePairing(deviceID, pairingID, pub, priv, PairingProtocolUnknown)
}

// SavePairing stores credentials together with the protocol which actually
// completed. New production call sites should use this instead of Save.
func (cs *CredentialStore) SavePairing(deviceID string, pairingID string, pub ed25519.PublicKey, priv ed25519.PrivateKey, protocol PairingProtocol) error {
	if protocol != PairingProtocolHAP && protocol != PairingProtocolRaw {
		return fmt.Errorf("cannot save unknown pairing protocol %q", protocol)
	}
	return cs.savePairing(deviceID, pairingID, pub, priv, protocol)
}

func (cs *CredentialStore) savePairing(deviceID string, pairingID string, pub ed25519.PublicKey, priv ed25519.PrivateKey, protocol PairingProtocol) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	creds, err := cs.backend.Lookup(deviceID)
	if err != nil {
		return err
	}
	if creds == nil {
		creds = &SavedCredentials{}
	}
	creds.PairingID = pairingID
	creds.Ed25519Public = append([]byte(nil), pub...)
	creds.Ed25519Seed = append([]byte(nil), priv.Seed()...)
	if protocol != PairingProtocolUnknown {
		creds.PairingProtocol = protocol
	}
	return cs.backend.Save(deviceID, creds)
}

// SavePassword stores the receiver's configured password for a device. An
// empty password is ignored.
func (cs *CredentialStore) SavePassword(deviceID, password string) error {
	if password == "" {
		return nil
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	creds, err := cs.backend.Lookup(deviceID)
	if err != nil {
		return err
	}
	if creds == nil {
		creds = &SavedCredentials{}
	}
	creds.Password = password
	return cs.backend.Save(deviceID, creds)
}

// SaveRestoreToken stores a Wayland screencast restore token for a device.
func (cs *CredentialStore) SaveRestoreToken(deviceID, restoreToken string) error {
	if restoreToken == "" {
		return nil
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	creds, err := cs.backend.Lookup(deviceID)
	if err != nil {
		return err
	}
	if creds == nil {
		creds = &SavedCredentials{}
	}
	creds.RestoreToken = restoreToken
	return cs.backend.Save(deviceID, creds)
}

// SaveCompanion stores a device's Companion pairing.
func (cs *CredentialStore) SaveCompanion(deviceID string, companion *CompanionCredentials) error {
	if !companion.Valid() {
		return fmt.Errorf("cannot save incomplete companion credentials")
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	creds, err := cs.backend.Lookup(deviceID)
	if err != nil {
		return err
	}
	if creds == nil {
		creds = &SavedCredentials{}
	}
	saved := *companion
	creds.Companion = &saved
	return cs.backend.Save(deviceID, creds)
}

// Forget removes everything saved for a device: its pairings, password and
// screencast restore token.
func (cs *CredentialStore) Forget(deviceID string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.backend.Delete(deviceID)
}

// fileBackend stores credentials as a JSON file on disk.
type fileBackend struct {
	path    string
	devices map[string]*SavedCredentials
}

func newFileBackend(path string) (*fileBackend, error) {
	fb := &fileBackend{
		path:    path,
		devices: make(map[string]*SavedCredentials),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fb, nil
		}
		return nil, fmt.Errorf("read credential store: %w", err)
	}
	if err := json.Unmarshal(data, &fb.devices); err != nil {
		return nil, fmt.Errorf("unmarshal credential store: %w", err)
	}
	return fb, nil
}

func (fb *fileBackend) Lookup(deviceID string) (*SavedCredentials, error) {
	return fb.devices[deviceID], nil
}

func (fb *fileBackend) Save(deviceID string, creds *SavedCredentials) error {
	fb.devices[deviceID] = creds
	return fb.persist()
}

func (fb *fileBackend) Delete(deviceID string) error {
	if _, ok := fb.devices[deviceID]; !ok {
		return nil
	}
	delete(fb.devices, deviceID)
	return fb.persist()
}

func (fb *fileBackend) persist() error {
	dir := filepath.Dir(fb.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create credential dir: %w", err)
	}
	data, err := json.MarshalIndent(fb.devices, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credential store: %w", err)
	}
	if err := os.WriteFile(fb.path, data, 0600); err != nil {
		return fmt.Errorf("write credential store: %w", err)
	}
	return nil
}
