package hap

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Credentials holds the long-term keys for a paired receiver.
// Stored as JSON at ~/.config/foxcast/credentials/<deviceID>.json.
type Credentials struct {
	DeviceID        string `json:"deviceID"`        // receiver MAC address (from mDNS)
	DevicePairingID string `json:"devicePairingID"` // receiver pairing identifier from M6
	DeviceLTPK      []byte `json:"deviceLTPK"`      // receiver Ed25519 public key (32 bytes)
	ClientID        string `json:"clientID"`        // our UUID pairing identifier
	ClientLTPK      []byte `json:"clientLTPK"`      // our Ed25519 public key (32 bytes)
	ClientLTSK      []byte `json:"clientLTSK"`      // our Ed25519 private key (64 bytes)
}

// credentialsDir returns the directory for credential files.
func credentialsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "foxcast", "credentials"), nil
}

// credentialsPath returns the file path for a given deviceID.
func credentialsPath(deviceID string) (string, error) {
	dir, err := credentialsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, deviceID+".json"), nil
}

// LoadCredentials loads saved credentials for the given deviceID.
// Returns (nil, nil) if no credentials file exists yet.
func LoadCredentials(deviceID string) (*Credentials, error) {
	path, err := credentialsPath(deviceID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hap: read credentials: %w", err)
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("hap: parse credentials: %w", err)
	}
	return &creds, nil
}

// SaveCredentials writes credentials to disk, creating directories as needed.
func SaveCredentials(creds *Credentials) error {
	dir, err := credentialsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("hap: create credentials dir: %w", err)
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("hap: marshal credentials: %w", err)
	}
	path, err := credentialsPath(creds.DeviceID)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("hap: write credentials: %w", err)
	}
	return nil
}

// GenerateClientKeys creates a new Ed25519 key pair and UUID for use in pair-setup.
func GenerateClientKeys() (id string, ltpk ed25519.PublicKey, ltsk ed25519.PrivateKey, err error) {
	ltpk, ltsk, err = ed25519.GenerateKey(nil)
	if err != nil {
		return "", nil, nil, fmt.Errorf("hap: generate Ed25519 keys: %w", err)
	}
	id = newUUID()
	return id, ltpk, ltsk, nil
}

// newUUID generates a random UUID v4.
func newUUID() string {
	var uuid [16]byte
	if _, err := randFill(uuid[:]); err != nil {
		panic("hap: generate UUID: " + err.Error())
	}
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(uuid[0:4]),
		hex.EncodeToString(uuid[4:6]),
		hex.EncodeToString(uuid[6:8]),
		hex.EncodeToString(uuid[8:10]),
		hex.EncodeToString(uuid[10:16]),
	)
}
