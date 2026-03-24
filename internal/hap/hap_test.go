package hap

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"
)

// ---- HKDF ----------------------------------------------------------------

func TestDeriveKey_KnownVector(t *testing.T) {
	// Vector generated from pyatv's hkdf_expand via:
	//   HKDF(SHA-512, ikm=0x00..0x1f, salt="Pair-Setup-Encrypt-Salt",
	//        info="Pair-Setup-Encrypt-Info", length=32)
	ikm := make([]byte, 32)
	for i := range ikm {
		ikm[i] = byte(i)
	}
	want, _ := hex.DecodeString("52890146745a52e57b82b859a7a3679c7f3d40bb295b055a0c8fa8af92a3746d")
	got := DeriveKey(ikm, "Pair-Setup-Encrypt-Salt", "Pair-Setup-Encrypt-Info")
	if !bytes.Equal(got, want) {
		t.Fatalf("DeriveKey: got %x\nwant %x", got, want)
	}
}

// ---- ChaCha20 ------------------------------------------------------------

func TestSealOpenMsg_KnownVector(t *testing.T) {
	// Vector generated from pyatv Chacha20Cipher8byteNonce with explicit nonce.
	// key=0x00..0x1f, nonce8="PS-Msg05", plain=b"hello world test"
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	plain, _ := hex.DecodeString("68656c6c6f20776f726c642074657374")
	wantCT, _ := hex.DecodeString("5c9fee01068c5a49c643b80c062c838ee777fa8e20015acc51b4e754e7821335")

	ct, err := SealMsg(key, "PS-Msg05", plain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ct, wantCT) {
		t.Fatalf("SealMsg: got %x\nwant %x", ct, wantCT)
	}

	dec, err := OpenMsg(key, "PS-Msg05", ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, plain) {
		t.Fatalf("OpenMsg: got %x want %x", dec, plain)
	}
}

func TestSealOpenMsg_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 100)
	}
	plain := []byte("round-trip payload for pair-verify")
	ct, err := SealMsg(key, "PV-Msg02", plain)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := OpenMsg(key, "PV-Msg02", ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, plain) {
		t.Fatalf("round-trip mismatch: got %q want %q", dec, plain)
	}
}

func TestEncryptDecryptFrame_KnownVector(t *testing.T) {
	// Vector from pyatv HAPSession.encrypt with counter=0.
	// key=0x20..0x3f, plain=b"test frame data"
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 32)
	}
	plain, _ := hex.DecodeString("74657374206672616d652064617461")
	wantFrame, _ := hex.DecodeString("0f00f4062ca833eea57cf74dd98584fd2f929072d4f2a823272207439eb7990f68")

	frame, err := EncryptFrame(key, 0, plain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame, wantFrame) {
		t.Fatalf("EncryptFrame: got  %x\nwant %x", frame, wantFrame)
	}

	dec, err := DecryptFrame(key, 0, frame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, plain) {
		t.Fatalf("DecryptFrame: got %x want %x", dec, plain)
	}
}

func TestEncryptDecryptFrame_MultipleCounters(t *testing.T) {
	key := make([]byte, 32)
	messages := [][]byte{
		[]byte("first message"),
		[]byte("second"),
		bytes.Repeat([]byte{0xAB}, 1024), // max frame size
	}
	for i, msg := range messages {
		frame, err := EncryptFrame(key, uint64(i), msg)
		if err != nil {
			t.Fatalf("encrypt counter %d: %v", i, err)
		}
		dec, err := DecryptFrame(key, uint64(i), frame)
		if err != nil {
			t.Fatalf("decrypt counter %d: %v", i, err)
		}
		if !bytes.Equal(dec, msg) {
			t.Fatalf("counter %d: round-trip mismatch", i)
		}
		// Wrong counter must fail.
		_, err = DecryptFrame(key, uint64(i+1), frame)
		if err == nil {
			t.Fatalf("counter %d: decryption with wrong counter should fail", i)
		}
	}
}

func TestEncryptFrame_TooLarge(t *testing.T) {
	key := make([]byte, 32)
	_, err := EncryptFrame(key, 0, make([]byte, 1025))
	if err == nil {
		t.Fatal("expected error for oversized frame")
	}
}

// ---- SRP ----------------------------------------------------------------

// srpServerProcess computes the server's view of the SRP exchange to
// allow round-trip testing without a network connection.
//
// Returns: (serverPublicB []byte, serverProofM2 []byte, serverSessionKey []byte)
func srpServerProcess(username, password string, salt []byte, clientPublicA *big.Int, bPriv *big.Int) ([]byte, []byte, []byte, error) {
	// x = SHA-512(salt || SHA-512("username:password"))
	innerHash := srpHashBytes([]byte(username + ":" + password))
	x := srpHash(salt, innerHash)

	// v = g^x mod N
	v := new(big.Int).Exp(srpG, x, srpN)

	// B = (k*v + g^b) mod N
	kv := new(big.Int).Mul(srpK, v)
	gb := new(big.Int).Exp(srpG, bPriv, srpN)
	B := new(big.Int).Add(kv, gb)
	B.Mod(B, srpN)

	// u = SHA-512(PAD(A) || PAD(B))
	u := srpHash(srpPad(clientPublicA), srpPad(B))

	// S = (A * v^u)^b mod N
	vu := new(big.Int).Exp(v, u, srpN)
	av := new(big.Int).Mul(clientPublicA, vu)
	av.Mod(av, srpN)
	S := new(big.Int).Exp(av, bPriv, srpN)

	// K = SHA-512(S)
	K := srpHashBytes(S.Bytes())

	// M1 check (server must verify this matches client's M1)
	hN := srpHash(srpN.Bytes())
	hG := srpHash(srpG.Bytes())
	hNxorG := new(big.Int).Xor(hN, hG)
	hI := srpHash([]byte(username))
	M1 := srpHashBytes(hNxorG.Bytes(), hI.Bytes(), salt, clientPublicA.Bytes(), B.Bytes(), K)

	// M2 = SHA-512(A || M1 || K)
	M2 := srpHashBytes(clientPublicA.Bytes(), M1, K)

	return B.Bytes(), M2, K, nil
}

func TestSRP_RoundTrip(t *testing.T) {
	username := "Pair-Setup"
	password := "1111"
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}

	// Fixed private keys for reproducibility.
	aPriv := new(big.Int).SetBytes(func() []byte {
		b := make([]byte, 32)
		for i := range b {
			b[i] = byte(i + 1)
		}
		return b
	}())
	bPriv := new(big.Int).SetBytes(func() []byte {
		b := make([]byte, 32)
		for i := range b {
			b[i] = byte(i + 33)
		}
		return b
	}())

	client := NewSRPClient(username, password, aPriv)

	serverB, serverM2, serverK, err := srpServerProcess(username, password, salt, client.A, bPriv)
	if err != nil {
		t.Fatal(err)
	}

	if err := client.Process(salt, serverB); err != nil {
		t.Fatal(err)
	}

	// Client and server must agree on session key K.
	if !bytes.Equal(client.SessionKey(), serverK) {
		t.Fatalf("session key mismatch:\nclient %x\nserver %x", client.SessionKey(), serverK)
	}

	// Server must accept client's M1 proof.
	// Recompute server M1 using client's M1 and compare.
	// (Server verifies by computing its own M1 and comparing with received.)
	// Here we verify via M2: server computed M2 using its M1, client verifies.
	if !client.VerifyServerProof(serverM2) {
		t.Fatal("client could not verify server proof M2")
	}
}

func TestSRP_KnownVector(t *testing.T) {
	// Vectors generated from srptools via pyatv with:
	//   username="Pair-Setup", PIN="1111"
	//   salt=0x000102...0f (16 bytes)
	//   client private a = bytes(range(1,33)) interpreted as big-endian
	//   server B computed from server private b = bytes(range(33,65))
	username := "Pair-Setup"
	password := "1111"
	salt, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")

	aPriv := new(big.Int).SetBytes(func() []byte {
		b := make([]byte, 32)
		for i := range b {
			b[i] = byte(i + 1)
		}
		return b
	}())

	serverB, _ := hex.DecodeString("6df6fe7dc31cea6fb1c2e9c486b9ab381db0e2a99059617c7eef2466445fe2fb" +
		"81dabc3f18fa305b8e1cfa61a61f461f12aedb171caae625a134d6944c398eb6" +
		"5826ddd54ab1d4a471ca4472e707e4e5af66cd86a39f899beeac2bccf9e34e22" +
		"6edc6b102504e0e98c3b1bcc253a9cc2cf0405c2e5bf74c8ccc5f45b31f70263" +
		"5337353e9aacbc0ab5e4c4852a405b6b1476efaadb66c2e4f53bbd8ac1b91a20" +
		"7db005aded31c66627bf3fe6c5f8edeb90fc23384f55d9496d38cfefe02ba6ca" +
		"a9845017cbfb6b06df9ba9eecaf7d3d508cee66283bc43ee6055a7eee6fbec32" +
		"f3d3f7ae4dd0eedf3d22894f61b77526a1e0f2de16fb1ee2448687afeb336b49" +
		"ca3de1cd6f9620582c36df2f388c5de0560579ad9a23120fa77e2a5524f7aed8" +
		"f9f3fd2871556f0bd6938fc67f98cb5993378b65b808a357bffaa4e012c3b845" +
		"5d3fe6c0d2e99536c81fde3c6bc4cfc92aabd9d0ec07bda9e1c8decd4246cb07" +
		"66d4e5c80326f8b1d11c76aec160dd98642938217dfc93827e6c3db72153ab63")

	wantK, _ := hex.DecodeString("843475dc53e09ec42c480c88c2b514e03a820791485f9bbbb6c32eb30d48a341" +
		"ea61e145e0f7b7742db22cc2e7cc80a7a4684322f8b7b73237f9da48eda4a178")
	wantM1, _ := hex.DecodeString("2327dffb529da71e0b872c544e78433f877962084e134b0509d73e31785e1b48" +
		"b25d3f7d02820ac5a06c5813bb850550059852d60fa20317b68bb2ea76cea244")

	client := NewSRPClient(username, password, aPriv)
	if err := client.Process(salt, serverB); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(client.SessionKey(), wantK) {
		t.Fatalf("K mismatch:\ngot  %x\nwant %x", client.SessionKey(), wantK)
	}
	if !bytes.Equal(client.Proof(), wantM1) {
		t.Fatalf("M1 mismatch:\ngot  %x\nwant %x", client.Proof(), wantM1)
	}
}
