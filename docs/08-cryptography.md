# Cryptography Reference

## Algorithm Summary

| Algorithm | Usage |
|-----------|-------|
| SRP-6a (3072-bit, SHA-512, g=5) | HAP pair-setup (modern Apple TV) |
| SRP-6a (2048-bit, SHA-1) | Legacy "fruit" pairing (tvOS < 10.2); not implemented by foxCast |
| X25519 (Curve25519) | Ephemeral ECDH key exchange in pair-verify |
| Ed25519 | Long-term device identity keys; signature verification |
| ChaCha20-Poly1305 | HAP session encryption (control channel), RTP audio (AirPlay 2), mirror video after HAP pair-verify |
| HKDF-SHA512 | All key derivation throughout HAP |
| AES-128-CTR | Mirror video when the control channel is plaintext; raw (legacy) pair-verify signatures |
| AES-128-CBC | RAOP audio stream payload (AirPlay 1, deprecated) |
| AES-128-GCM | HAP M5-M6 pair-setup credential exchange (some variants) |
| SHA-1 | HMAC-SHA1 tag in the FairPlay `ekey` record |
| SHA-512 | SRP hashing; mirror AES-CTR key/IV derivation; FairPlay key mixing with the pair-verify secret; raw pair-verify AES key/IV |
| RSA-2048-OAEP | AirPlay 1 SDP AES key encryption (legacy, not for Apple TV) |

---

## SRP-6a Parameters

HAP pair-setup uses SRP-6a as specified in RFC 5054, with non-standard Apple modifications.

### HAP (Modern Apple TV, HomeKit Pairing)

- Prime: RFC 5054 Appendix A.4, 3072-bit
- Generator: g = 5
- Hash: SHA-512
- Username: `"Pair-Setup"`

### Session Key

HAP uses the standard SRP-6a session key `K = SHA-512(S)` (64 bytes, `S` in its natural unpadded form), which foxCast implements (`newSRPClientSession`) and Apple TVs accept. The interleaved form `K = H(S || 0x00000000) || H(S || 0x00000001)` belongs to the legacy SHA-1 fruit pairing below, not to HAP.

foxCast also pads `A` and `B` to 384 bytes for `u = H(PAD(A) || PAD(B))` and `k = H(N || PAD(g))`, but uses unpadded values in the proofs `M1 = H(H(N) XOR H(g), H(I), s, A, B, K)` and `M2 = H(A, M1, K)`.

### Legacy Fruit Pairing (tvOS < 10.2)

- Prime: RFC 5054 Appendix A.1, 2048-bit
- Hash: SHA-1
- Uses binary plist format instead of TLV8

---

## HKDF

All key derivation uses HKDF-SHA512 (RFC 5869) with output length 32 bytes unless otherwise noted.

```go
// Pseudocode
key = HKDF(
    hash=SHA512,
    ikm=input_key_material,
    salt=[]byte(salt_string),
    info=[]byte(info_string),
    length=32,
)
```

See [authentication.md](03-authentication.md) for the complete table of salt/info strings.

---

## ChaCha20-Poly1305

Used for:
1. HAP session encryption (all RTSP traffic post pair-verify)
2. AirPlay 2 RTP audio payload encryption

### HAP Session Frames

- Nonce: 64-bit counter (uint64, little-endian), zero-padded to 12 bytes
- Counter increments per frame (separately for send and receive)
- AAD: 2-byte plaintext length field
- Max frame size: 1024 bytes plaintext

### RTP Audio Packets

- Key: foxCast generates a random 32-byte key per stream and sends it to the receiver in the stream's `shk` (with `streamConnectionKeyUseStreamEncryptionKey`); it is not HKDF-derived from the HAP session
- Nonce: 8 bytes appended to the end of the RTP packet; prepend 4 zero bytes to form 12-byte ChaCha nonce
- AAD: RTP header bytes 4–11 (timestamp + SSRC)

> **Important:** The 8-byte nonce is transmitted with the packet (last 8 bytes). It is **not** auto-incremented by the ChaCha cipher — use the transmitted value directly.

---

## AES-128-CTR (Mirror Video)

Used when the control channel is plaintext (raw pair-verify). Key and IV are derived from the stream key and `streamConnectionID` using SHA-512 (as in UxPlay's `mirror_buffer.c`; `deriveVideoKeys`):

```
key_material = SHA-512("AirPlayStreamKey" + decimal_string(streamConnectionID) + stream_key)
iv_material  = SHA-512("AirPlayStreamIV"  + decimal_string(streamConnectionID) + stream_key)

aes_key = key_material[0:16]
aes_iv  = iv_material[0:16]
```

Where `stream_key` is the 16-byte FairPlay stream key (see [03-authentication.md](03-authentication.md)). The CTR keystream carries over between frames: a frame's trailing partial block leaves unused keystream that encrypts the start of the next frame, after which the counter restarts on a block boundary. `mirror -direct-key` skips the derivation for debugging.

## ChaCha20-Poly1305 (Mirror Video over HAP)

After HAP pair-verify, the video stream is encrypted with ChaCha20-Poly1305 instead, keyed with HKDF-SHA512 (`deriveChaChaKey`):

```
ikm  = pair-verify X25519 shared secret (FairPlay AES key if none)
salt = "DataStream-Salt" + decimal_string(streamConnectionID)
info = "DataStream-Output-Encryption-Key"
```

---

## FairPlay v3 (Custom Cipher)

FairPlay v3 is a proprietary algorithm (not based on any standard cryptographic primitive). It uses:
- Hardcoded lookup tables
- Modified-MD5 operations (SAPHash)
- XOR operations
- A custom mixing function

The algorithm produces a 16-byte AES key from the 164-byte keymsg exchanged during `POST /fp-setup`. This key is used to encrypt the audio stream AES key.

foxCast's sender implementation is native Go (`fpsap.go`, `fairplay_*.go`), tested against the playfair reference vectors. It generates the raw stream key itself and wraps it in the 72-byte `ekey` (FPLY header, random mask, key length, HMAC-SHA1 tag, AES-wrapped masked key).

Reference implementations:
- [openairplay/airplay2-receiver](https://github.com/openairplay/airplay2-receiver) — Python
- [FDH2/UxPlay](https://github.com/FDH2/UxPlay) — C

---

## AES-128-CBC (AirPlay 1 Audio, Legacy)

- Key and IV from SDP ANNOUNCE `a=rsaaeskey:` and `a=aesiv:` fields
- Applied per RTP packet to the payload only (not the 12-byte RTP header)
- Only 16-byte-aligned portion is encrypted; remaining tail bytes are appended unencrypted

Not used by modern Apple TV. Documented for completeness.

---

## Key Storage

Long-term keys that must be persisted between sessions (general HAP view):

| Key | Description | Storage location |
|-----|-------------|-----------------|
| Client LTSK | Ed25519 long-term secret key (32 bytes) | Per-application secret |
| Client LTPK | Ed25519 long-term public key (32 bytes) | Derived from LTSK |
| Client pairing ID | UUID string | Per-application config |
| Receiver LTPK | Ed25519 long-term public key of receiver (32 bytes) | Per-receiver, from pair-setup M6 |
| Receiver pairing ID | UUID string from receiver | Per-receiver, from pair-setup M6 |

The receiver's LTPK and pairing ID should be validated against the `pk` TXT record in mDNS on each connection.

foxCast stores one entry per receiver AirPlay device ID in `$XDG_CONFIG_HOME/foxcast/credentials.json` (default `~/.config`, mode 0600), or in the system keyring (service `foxcast`) with `-cred-backend keyring`. An entry holds our pairing ID, Ed25519 public key and 32-byte seed, the pair-verify protocol that completed (`hap` or `raw`), and optionally the receiver's configured password, a screencast restore token and the Companion pairing. The receiver's LTPK and pairing ID are not stored.
