# Authentication and Pairing

AirPlay 2 uses HomeKit Accessory Protocol (HAP) for authentication. There are several authentication paths depending on receiver capability flags.

## Authentication Method Selection

| Condition | Method |
|-----------|--------|
| Bits 12 or 14 set (FairPlay) | FairPlay fp-setup — must run alongside HAP pairing |
| Bit 46 set (HomeKitPairing) | HAP pair-setup + pair-verify (stored long-term credentials) |
| Bit 48 set (TransientPairing) | Transient pairing (fixed PIN `3939`, no credential storage) |
| Bit 27 set (LegacyPairing) | Legacy SRP (older receivers, tvOS < 10.2) |

Modern Apple TVs require **HAP pair-verify on every connection**, using credentials established in a prior pair-setup. Transient pairing can be used when bit 48 is set without needing stored credentials.

---

## FairPlay Authentication (Bits 12/14)

FairPlay v3 runs as a pre-authentication step before RTSP SETUP. It is required by most Apple TVs even alongside HAP pairing.

```
POST /fp-setup RTSP/1.0
Content-Type: application/octet-stream
Content-Length: 16
[16 bytes: byte[4] must be 0x03]

← 142-byte response (one of 4 hardcoded variants based on mode byte at offset 14)

POST /fp-setup RTSP/1.0
Content-Type: application/octet-stream
Content-Length: 164
[164 bytes: "FPLY" at bytes 0-3, version bytes 0x03,0x01]

← 32-byte response:
  bytes  0-11: fixed header {0x46,0x50,0x4C,0x59, 0x03,0x01, 0x04,0x00, 0x00,0x00,0x00,0x14}
  bytes 12-31: echo of request bytes 144-163
```

FairPlay v3 derives a 16-byte AES key through a custom cipher (proprietary modified-MD5 / SAPHash / XOR construction — not a standard algorithm). This key is used to encrypt the audio stream AES key.

> **Note:** The FairPlay algorithm is implemented in the UxPlay source and the openairplay/airplay2-receiver Python code. It involves hardcoded lookup tables and is not reducible to any standard cryptographic primitive.

---

## HAP Pair-Setup (One-Time, Stores Long-Term Credentials)

Pair-setup must be done once per sender/receiver pair. The receiver displays a PIN that the user enters on the sender side. Long-term keys (LTPK/LTSK) are stored after completion.

All pair-setup messages use `Content-Type: application/octet-stream` with TLV8 encoding (see [data-formats.md](07-data-formats.md)).

### Step 1: Trigger PIN display

```
POST /pair-pin-start RTSP/1.0
```

### Step 2: SRP Authentication (M1–M4)

Uses SRP-6a with a 3072-bit prime (RFC 5054 Appendix A.4), SHA-512, generator g=5. Username = `"Pair-Setup"`.

**M1 (Client → Server):**
```
TLV8 {Method=0x00 (pair-setup), State=0x01 (M1)}
```

**M2 (Server → Client):**
```
TLV8 {State=0x02 (M2), PublicKey=B[384 bytes SRP], Salt=[16 bytes random]}
```

**M3 (Client → Server):**
```
TLV8 {State=0x03 (M3), PublicKey=A[SRP], Proof=M1_client_proof}
```

**M4 (Server → Client):**
```
TLV8 {State=0x04 (M4), Proof=M2_server_proof}
```

> **Apple SRP Quirk:** Apple's session key derivation is non-standard:
> `K = SHA-512(S || 0x00000000) || SHA-512(S || 0x00000001)` producing a 128-byte key
> (standard SRP uses a single SHA hash of S).

### Step 3: Exchange Long-Term Keys (M5–M6)

Derive encryption key for this exchange:
```
session_key = HKDF-SHA512(
  ikm=SRP_session_key,
  salt="Pair-Setup-Encrypt-Salt",
  info="Pair-Setup-Encrypt-Info",
  len=32
)
```

**M5 (Client → Server):**
```
inner_payload = TLV8 {
  Identifier = <client_pairing_id>,     // UUID string
  PublicKey  = <client_Ed25519_LTPK>,   // 32 bytes
  Signature  = Ed25519_sign(
    key=client_LTSK,
    msg=HKDF(SRP_session_key, "Pair-Setup-Controller-Sign-Salt",
             "Pair-Setup-Controller-Sign-Info", 32)
      || client_pairing_id
      || client_Ed25519_LTPK
  )
}

TLV8 {
  State = 0x05 (M5),
  EncryptedData = ChaCha20Poly1305_seal(
    key=session_key,
    nonce="PS-Msg05",  // padded to 12 bytes
    aad="",
    plaintext=inner_payload
  )   // ciphertext || 16-byte tag
}
```

> **AES-GCM IV note:** If AES-GCM is used instead of ChaCha (some variants), the last byte of
> the HKDF-derived IV must be incremented by 1 before use.

**M6 (Server → Client):** Same structure with server's credentials. Verify the server's Ed25519 signature against the server LTPK from the TXT record `pk` field.

---

## HAP Pair-Verify (Every Connection)

Run after (or instead of) pair-setup to establish the session encryption keys. Uses X25519 ECDH + Ed25519 signatures.

All pair-verify messages use `Content-Type: application/octet-stream` with TLV8 encoding.

**M1 (Client → Server):**
```
TLV8 {
  State = 0x01 (M1),
  PublicKey = client_X25519_ephemeral_pub  // 32 bytes
}
```

**M2 (Server → Client):**
```
X25519_shared = X25519(client_X25519_priv, server_X25519_pub)

verify_key = HKDF-SHA512(
  ikm=X25519_shared,
  salt="Pair-Verify-Encrypt-Salt",
  info="Pair-Verify-Encrypt-Info",
  len=32
)

server_inner = TLV8 {
  Identifier = server_pairing_id,
  Signature = Ed25519_sign(
    key=server_LTSK,
    msg=server_X25519_pub || server_pairing_id || client_X25519_pub
  )
}

TLV8 {
  State = 0x02 (M2),
  PublicKey = server_X25519_pub,  // 32 bytes
  EncryptedData = ChaCha20Poly1305_seal(
    key=verify_key, nonce="PV-Msg02",
    plaintext=server_inner
  )
}
```

Verify: X25519_shared using client's ephemeral private + server's X25519 public, then verify server's Ed25519 signature against the `pk` from the mDNS TXT record.

**M3 (Client → Server):**
```
client_inner = TLV8 {
  Identifier = client_pairing_id,
  Signature = Ed25519_sign(
    key=client_LTSK,
    msg=client_X25519_pub || client_pairing_id || server_X25519_pub
  )
}

TLV8 {
  State = 0x03 (M3),
  EncryptedData = ChaCha20Poly1305_seal(
    key=verify_key, nonce="PV-Msg03",
    plaintext=client_inner
  )
}
```

**M4 (Server → Client):**
```
TLV8 {State = 0x04 (M4)}
```
Success. All subsequent RTSP traffic is now HAP-encrypted.

---

## Transient Pairing (Bit 48)

Runs only M1–M4 of pair-setup with hardcoded PIN `"3939"`. No stored credentials. Derives session encryption keys but does not exchange long-term keys. Suitable for audio/video playback without prior HomeKit pairing.

The SRP password and flow are not consistent across implementations:

- doubletake: empty password, then M5/M6 and HAP pair-verify as for full pairing.
- pyatv: `"3939"`, stopping after M4; the control channel is keyed directly from the SRP session key K (`Control-Salt` / `Control-{Write,Read}-Encryption-Key`), with no pair-verify.
- Password-protected Apple TV (AppleTV11,1, observed): rejects `""` at M4 with error 2 (authentication), accepts the configured password at M4, then drops the connection when M5 is sent, so it too expects the M4-only flow.

foxCast tries the configured password first, then `""`, then `"3939"` (`sender.TransientSetupCodes`), on a fresh connection per attempt. A non-empty code uses the M4-only flow; the empty code keeps doubletake's full flow.

Because transient pairing stores no keys, a password-protected receiver needs its password on every connection. The CLI saves it in the receiver's credential entry (`password`, next to the pairing keys) once pairing succeeds, or once a mirror SETUP that challenged for it succeeds, and reuses it on later launches. `-code`/`$FOXCAST_CODE` overrides the saved value; `-pair` ignores it so a changed password can be re-entered. One-time on-screen PINs are never saved.

---

## HAP Session Encryption

After successful pair-verify, **all subsequent RTSP messages are encrypted** (both request and response bodies and headers, including status line).

### Key Derivation

```
write_key = HKDF-SHA512(
  ikm=X25519_shared_secret,  // from pair-verify
  salt="Control-Salt",
  info="Control-Write-Encryption-Key",
  len=32
)
read_key = HKDF-SHA512(
  ikm=X25519_shared_secret,
  salt="Control-Salt",
  info="Control-Read-Encryption-Key",
  len=32
)
```

The event channel (port from initial SETUP response) uses separate keys:
```
salt="Events-Salt"
info="Events-Write-Encryption-Key" / "Events-Read-Encryption-Key"
```

### Encrypted Frame Format

Each RTSP message is wrapped in one or more frames:
```
Bytes 0-1:   plaintext length (uint16 little-endian, max 1024 bytes per frame)
Bytes 2-N:   ChaCha20-Poly1305 ciphertext
Bytes N to N+16: 16-byte Poly1305 authentication tag

AAD = bytes 0-1 (the length field)
Nonce = send_counter (uint64 little-endian, zero-padded to 12 bytes)
```

Counter increments per frame, separately for each direction. Messages larger than 1024 bytes are split across multiple frames.

---

## HKDF Salt/Info Strings (Reference)

These exact strings must be used; any deviation will produce incorrect keys:

| Purpose | salt | info |
|---------|------|------|
| Pair-setup encryption | `"Pair-Setup-Encrypt-Salt"` | `"Pair-Setup-Encrypt-Info"` |
| Pair-setup controller sign | `"Pair-Setup-Controller-Sign-Salt"` | `"Pair-Setup-Controller-Sign-Info"` |
| Pair-verify encryption | `"Pair-Verify-Encrypt-Salt"` | `"Pair-Verify-Encrypt-Info"` |
| Control channel (write) | `"Control-Salt"` | `"Control-Write-Encryption-Key"` |
| Control channel (read) | `"Control-Salt"` | `"Control-Read-Encryption-Key"` |
| Events channel (write) | `"Events-Salt"` | `"Events-Write-Encryption-Key"` |
| Events channel (read) | `"Events-Salt"` | `"Events-Read-Encryption-Key"` |
| DataStream channel | `"DataStream-Salt"` | `"DataStream-Output-Encryption-Key"` / `"DataStream-Input-Encryption-Key"` |

HKDF function: SHA-512, output length 32 bytes for all cases above.
