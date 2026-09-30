package sender

// In-process Companion service for tests and foxCast-test-receiver: PIN
// pair-setup, pair-verify, and the session and app-launch commands the
// client sends. Launched targets are recorded rather than opened.

import (
	"bufio"
	"context"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"sync"

	"git.foxden.network/FoxDen/foxCast/internal/opack"
	"golang.org/x/crypto/chacha20poly1305"
)

// CompanionReceiverConfig configures a CompanionReceiver.
type CompanionReceiverConfig struct {
	// ListenAddress defaults to 127.0.0.1:0.
	ListenAddress string
	// PIN is the code pair-setup expects, as an Apple TV would display it.
	PIN string
	// Apps are the launchable apps, bundle ID to name.
	Apps   map[string]string
	Logger *log.Logger
	Debug  bool
}

// CompanionReceiver is a minimal Companion service.
type CompanionReceiver struct {
	cfg         CompanionReceiverConfig
	listener    net.Listener
	privateKey  ed25519.PrivateKey
	identifier  string
	controllers *receiverControllerStore

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	launched []string
	closed   bool
	wg       sync.WaitGroup
}

// NewCompanionReceiver opens the listener. Call Serve to accept clients.
func NewCompanionReceiver(cfg CompanionReceiverConfig) (*CompanionReceiver, error) {
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1:0"
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate companion identity: %w", err)
	}
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.ListenAddress, err)
	}
	return &CompanionReceiver{
		cfg:         cfg,
		listener:    listener,
		privateKey:  privateKey,
		identifier:  generateUUID(),
		controllers: newReceiverControllerStore(),
		conns:       make(map[net.Conn]struct{}),
	}, nil
}

// Addr returns the bound address.
func (r *CompanionReceiver) Addr() net.Addr { return r.listener.Addr() }

// Launched returns the URLs and bundle IDs clients have launched, in order.
func (r *CompanionReceiver) Launched() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.launched...)
}

// Serve accepts connections until ctx is cancelled or Close is called.
func (r *CompanionReceiver) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { _ = r.Close() })
	defer stop()
	r.logf("listening on %s", r.listener.Addr())
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept companion connection: %w", err)
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			conn.Close()
			return nil
		}
		r.conns[conn] = struct{}{}
		r.wg.Add(1)
		r.mu.Unlock()
		go func() {
			defer r.wg.Done()
			defer func() {
				r.mu.Lock()
				delete(r.conns, conn)
				r.mu.Unlock()
				conn.Close()
			}()
			if err := r.serveConn(conn); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				r.logf("connection %s failed: %v", conn.RemoteAddr(), err)
			}
		}()
	}
}

// Close stops the listener and all connections.
func (r *CompanionReceiver) Close() error {
	r.mu.Lock()
	r.closed = true
	err := r.listener.Close()
	for conn := range r.conns {
		conn.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (r *CompanionReceiver) logf(format string, args ...any) {
	if r.cfg.Debug {
		r.cfg.Logger.Printf("[COMPANION-RECEIVER] "+format, args...)
	}
}

func (r *CompanionReceiver) serveConn(conn net.Conn) error {
	pairing, err := newReceiverPairingState(receiverPairingConfig{
		identifier:  r.identifier,
		privateKey:  r.privateKey,
		pin:         r.cfg.PIN,
		controllers: r.controllers,
	})
	if err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	var writeAEAD, readAEAD cipher.AEAD
	var writeNonce, readNonce uint64
	send := func(frameType byte, message map[string]any) error {
		payload, err := opack.Marshal(message)
		if err != nil {
			return err
		}
		frame, err := sealCompanionFrame(writeAEAD, &writeNonce, frameType, payload)
		if err != nil {
			return err
		}
		_, err = conn.Write(frame)
		return err
	}

	for {
		frameType, payload, err := readCompanionFrame(reader, readAEAD, &readNonce)
		if err != nil {
			return err
		}
		value, err := opack.Unmarshal(payload)
		if err != nil {
			return err
		}
		message, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("frame holds %T", value)
		}
		r.logf("<< frame %d: %v", frameType, message)

		switch frameType {
		case companionFramePSStart, companionFramePSNext, companionFramePVStart, companionFramePVNext:
			data, _ := message[companionKeyPairingData].([]byte)
			verify := frameType == companionFramePVStart || frameType == companionFramePVNext
			var reply []byte
			replyType := byte(companionFramePSNext)
			if verify {
				replyType = companionFramePVNext
				reply, err = pairing.pairVerify(data)
			} else {
				if frameType == companionFramePSStart && r.cfg.PIN != "" {
					r.cfg.Logger.Printf("companion pairing PIN: %s", r.cfg.PIN)
				}
				reply, err = pairing.pairSetup(data)
			}
			if err != nil {
				return err
			}
			if err := send(replyType, map[string]any{companionKeyPairingData: reply}); err != nil {
				return err
			}
			if keys, ok := pairing.sessionKeys(); verify && ok {
				writeKey := hkdfSHA512(keys.sharedSecret, nil, []byte(companionServerEncryptInfo), chacha20poly1305.KeySize)
				readKey := hkdfSHA512(keys.sharedSecret, nil, []byte(companionClientEncryptInfo), chacha20poly1305.KeySize)
				if writeAEAD, err = chacha20poly1305.New(writeKey); err != nil {
					return err
				}
				if readAEAD, err = chacha20poly1305.New(readKey); err != nil {
					return err
				}
				writeNonce, readNonce = 0, 0
			}
		case companionFrameEOPACK:
			if readAEAD == nil {
				return errors.New("request before pair-verify")
			}
			if t, _ := asUint64(message[companionKeyType]); t != companionMessageRequest {
				continue
			}
			command, _ := message[companionKeyIdentifier].(string)
			content, _ := message[companionKeyContent].(map[string]any)
			reply := map[string]any{
				companionKeyType: companionMessageResponse,
				companionKeyXID:  message[companionKeyXID],
			}
			result, handleErr := r.handle(command, content)
			if handleErr != nil {
				reply[companionKeyErrorMsg] = handleErr.Error()
			} else {
				reply[companionKeyContent] = result
			}
			if err := send(companionFrameEOPACK, reply); err != nil {
				return err
			}
		default:
			r.logf("ignoring frame type %d", frameType)
		}
	}
}

func (r *CompanionReceiver) handle(command string, content map[string]any) (map[string]any, error) {
	switch command {
	case companionCommandSystemInfo, companionCommandSessionStop:
		return map[string]any{}, nil
	case companionCommandSessionStart:
		sid, err := rand.Int(rand.Reader, big.NewInt(1<<32))
		if err != nil {
			return nil, err
		}
		return map[string]any{companionKeySessionID: sid.Uint64()}, nil
	case companionCommandLaunchApp:
		target, _ := content[companionKeyLaunchURL].(string)
		if target == "" {
			target, _ = content[companionKeyLaunchBundleID].(string)
		}
		if target == "" {
			return nil, errors.New("nothing to launch")
		}
		r.mu.Lock()
		r.launched = append(r.launched, target)
		r.mu.Unlock()
		r.logf("launched %s", target)
		return map[string]any{}, nil
	case companionCommandAppList:
		apps := make(map[string]any, len(r.cfg.Apps))
		for id, name := range r.cfg.Apps {
			apps[id] = name
		}
		return apps, nil
	default:
		return nil, fmt.Errorf("unsupported command %q", command)
	}
}
