# Data Formats

## TLV8

Used for HAP pair-setup and pair-verify messages (`Content-Type: application/octet-stream`).

### Encoding

Each TLV element:
```
Byte 0:   tag    (1 byte)
Byte 1:   length (1 byte, max 255)
Bytes 2+: value  (length bytes)
```

Values longer than 255 bytes are fragmented: split into multiple TLV entries with the **same tag**, each fragment at most 255 bytes. A decoder concatenates consecutive same-tag fragments.

### Standard Tags

| Tag | Name | Type/Size |
|-----|------|-----------|
| 0x00 | Method | uint8 (0=pair-setup, 2=pair-verify) |
| 0x01 | Identifier | UTF-8 string |
| 0x02 | Salt | 16 bytes random |
| 0x03 | PublicKey | 32 bytes (X25519/Ed25519) or 384 bytes (SRP) |
| 0x04 | Proof | hash-length bytes |
| 0x05 | EncryptedData | ciphertext + 16-byte Poly1305 tag |
| 0x06 | State | uint8 (M1=0x01 through M6=0x06) |
| 0x07 | Error | uint8 error code |
| 0x09 | Permissions | uint8 |
| 0x0A | NumDevices | uint8 |

### Error Codes (tag 0x07)

| Code | Name |
|------|------|
| 0x01 | Unknown |
| 0x02 | Authentication (wrong PIN or signature mismatch) |
| 0x03 | Backoff (too many attempts) |
| 0x04 | MaxPeers (too many paired devices) |
| 0x05 | MaxTries |
| 0x06 | Unavailable |
| 0x07 | Busy |

---

## Apple Binary Plist

Most AirPlay 2 control messages use Apple Binary Property List format.

- `Content-Type: application/x-apple-binary-plist`
- Magic bytes: `62706c697374 3030` (`bplist00`)
- Standard format supported by Apple's `CoreFoundation` and cross-platform libraries

### Libraries

| Language | Library |
|----------|---------|
| Go | `howett.net/plist` |
| Rust | `plist` crate |
| Python | `biplist`, `plistlib` (stdlib) |
| C | `libplist` |

---

## DMAP (Digital Audio Access Protocol)

Used for audio metadata (`Content-Type: application/x-dmap-tagged`). Sent via `SET_PARAMETER`.

### Frame Format

Each field is a TLV with 4-byte code and 4-byte length:
```
Bytes 0-3: 4-char ASCII type code (e.g., "mlit", "minm", "asal")
Bytes 4-7: data length (uint32, big-endian)
Bytes 8+:  data (length bytes)
```

### Common Codes

| Code | Name | Type | Description |
|------|------|------|-------------|
| `mlit` | dmap.listingitem | container | Wraps a metadata item |
| `minm` | dmap.itemname | string | Track title |
| `asal` | daap.songalbum | string | Album name |
| `asar` | daap.songartist | string | Artist name |
| `ascp` | daap.songcomposer | string | Composer |
| `asdt` | daap.songdescription | string | Description |
| `assn` | daap.sortname | string | Sort name |
| `asgn` | daap.songgenre | string | Genre |
| `astm` | daap.songtime | uint32 | Duration (milliseconds) |
| `astn` | daap.songtracknumber | uint16 | Track number |

---

## SDP (AirPlay 1 Only)

AirPlay 1 audio uses standard RFC 4566 SDP in an RTSP ANNOUNCE, with Apple extensions.

### AirPlay-Specific SDP Fields

```
a=rtpmap:96 AppleLossless
a=fmtp:96 <spf> 0 <sample_size> 40 10 14 <channels> 255 0 0 <sample_rate>
a=rsaaeskey:<base64(RSA-OAEP encrypted 16-byte AES key, no padding)>
a=aesiv:<base64(16-byte AES IV, no padding)>
a=fpaeskey:<base64(FairPlay encrypted AES key)>
a=min-latency:<samples>
a=max-latency:<samples>
```

Note: base64 in SDP uses no padding characters (no trailing `=`).

> AirPlay 1 SDP is **not used** by modern Apple TVs. Document for reference only.

---

## RTP Header Reference

Standard RTP header (RFC 3550), 12 bytes:

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|V=2|P|X|  CC   |M|     PT      |       sequence number         |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                           timestamp                           |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|           synchronization source (SSRC) identifier           |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

For AirPlay audio: V=2, P=0, X=0, CC=0, M=0 (1 on first packet), PT=96, SSRC=0.

---

## RTSP Message Format

Standard RTSP/1.0 (RFC 2326) with additions:

- All requests include `CSeq: <incrementing integer>`
- AirPlay adds `X-Apple-Session-ID: <UUID>` header
- AirPlay 2 uses `SETUP`, `RECORD`, `TEARDOWN`, `SET_PARAMETER`, `FEEDBACK`, `FLUSHBUFFERED`, `SETRATEANCHORTIME` methods
- After HAP pair-verify, all RTSP traffic is encrypted in HAP frames (see [authentication.md](03-authentication.md))
