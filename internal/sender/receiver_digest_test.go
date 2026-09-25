package sender

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestReceiverDigestAuthDisabledWithoutPassword(t *testing.T) {
	auth, err := newReceiverDigestAuth("")
	if err != nil {
		t.Fatal(err)
	}
	if auth.enabled() {
		t.Fatal("empty password enabled receiver authentication")
	}
	if got := auth.challengeHeader(); got != "" {
		t.Fatalf("disabled challenge = %q, want empty", got)
	}
	if !auth.authorize("SETUP", "rtsp://host/1", "malformed") {
		t.Fatal("disabled receiver authentication rejected a request")
	}
	var nilAuth *receiverDigestAuth
	if nilAuth.enabled() || nilAuth.challengeHeader() != "" || !nilAuth.authorize("GET", "/info", "") {
		t.Fatal("nil receiver authentication is not disabled")
	}
}

func TestReceiverDigestAuthChallengeIsRandomAndStable(t *testing.T) {
	first, err := newReceiverDigestAuth("secret")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newReceiverDigestAuth("secret")
	if err != nil {
		t.Fatal(err)
	}
	if !first.enabled() {
		t.Fatal("configured password did not enable receiver authentication")
	}
	if first.realm != receiverDigestRealm {
		t.Fatalf("realm = %q, want %q", first.realm, receiverDigestRealm)
	}
	decoded, err := base64.RawStdEncoding.DecodeString(first.nonce)
	if err != nil {
		t.Fatalf("nonce is not raw base64: %v", err)
	}
	if len(decoded) != receiverDigestNonceSize {
		t.Fatalf("nonce entropy = %d bytes, want %d", len(decoded), receiverDigestNonceSize)
	}
	if first.nonce == second.nonce {
		t.Fatal("two receiver instances generated the same nonce")
	}

	challenge := first.challengeHeader()
	if challenge != first.challengeHeader() {
		t.Fatal("challenge changed within one receiver instance")
	}
	parsed, ok := parseDigestChallenge(challenge)
	if !ok || parsed.Realm != first.realm || parsed.Nonce != first.nonce {
		t.Fatalf("challenge %q did not round trip: %#v, %t", challenge, parsed, ok)
	}
}

func TestReceiverDigestAuthAuthorizesExactRequest(t *testing.T) {
	auth, err := newReceiverDigestAuth("secret")
	if err != nil {
		t.Fatal(err)
	}
	const method = "SETUP"
	const uri = "rtsp://127.0.0.1/1234"
	challenge := &digestChallenge{Realm: auth.realm, Nonce: auth.nonce}
	header := authorizationHeader(DigestUsername, auth.password, challenge, method, uri)
	if !auth.authorize(method, uri, header) {
		t.Fatalf("valid Authorization was rejected: %s", header)
	}

	response := digestResponse(DigestUsername, auth.realm, auth.password, auth.nonce, method, uri)
	bare := "digest username=" + DigestUsername +
		", REALM=" + auth.realm +
		", nonce=" + auth.nonce +
		", uri=" + uri +
		", response=" + strings.ToUpper(response) +
		", algorithm=md5"
	if !auth.authorize(method, uri, bare) {
		t.Fatalf("valid bare/case-insensitive Authorization was rejected: %s", bare)
	}

	commaURI := "rtsp://127.0.0.1/a,b"
	commaHeader := authorizationHeader(DigestUsername, auth.password, challenge, method, commaURI)
	if !auth.authorize(method, commaURI, commaHeader) {
		t.Fatal("quoted comma in URI was not preserved")
	}
}

func TestReceiverDigestAuthRejectsInvalidAuthorization(t *testing.T) {
	auth, err := newReceiverDigestAuth("secret")
	if err != nil {
		t.Fatal(err)
	}
	const method = "SETUP"
	const uri = "rtsp://127.0.0.1/1234"

	fields := map[string]string{
		"username":  DigestUsername,
		"realm":     auth.realm,
		"nonce":     auth.nonce,
		"uri":       uri,
		"response":  digestResponse(DigestUsername, auth.realm, auth.password, auth.nonce, method, uri),
		"algorithm": "MD5",
	}
	header := func(overrides map[string]string, omit ...string) string {
		values := make(map[string]string, len(fields))
		for key, value := range fields {
			values[key] = value
		}
		for key, value := range overrides {
			values[key] = value
		}
		for _, key := range omit {
			delete(values, key)
		}
		order := []string{"username", "realm", "nonce", "uri", "response", "algorithm", "qop", "cnonce", "nc", "unknown"}
		var parts []string
		for _, key := range order {
			if value, ok := values[key]; ok {
				parts = append(parts, key+`="`+value+`"`)
			}
		}
		return "Digest " + strings.Join(parts, ", ")
	}

	tests := []struct {
		name          string
		authorization string
		actualMethod  string
		actualURI     string
	}{
		{name: "empty", authorization: ""},
		{name: "wrong scheme", authorization: strings.Replace(header(nil), "Digest", "Basic", 1)},
		{name: "scheme without whitespace", authorization: strings.Replace(header(nil), "Digest ", "Digest", 1)},
		{name: "missing username", authorization: header(nil, "username")},
		{name: "missing realm", authorization: header(nil, "realm")},
		{name: "missing nonce", authorization: header(nil, "nonce")},
		{name: "missing uri", authorization: header(nil, "uri")},
		{name: "missing response", authorization: header(nil, "response")},
		{name: "wrong username", authorization: header(map[string]string{"username": "iTunes"})},
		{name: "wrong realm", authorization: header(map[string]string{"realm": "raop"})},
		{name: "wrong nonce", authorization: header(map[string]string{"nonce": "other"})},
		{name: "wrong uri field", authorization: header(map[string]string{"uri": "rtsp://127.0.0.1/other"})},
		{name: "wrong response", authorization: header(map[string]string{"response": strings.Repeat("0", 32)})},
		{name: "wrong password", authorization: authorizationHeader(DigestUsername, "wrong", &digestChallenge{Realm: auth.realm, Nonce: auth.nonce}, method, uri)},
		{name: "non hex response", authorization: header(map[string]string{"response": strings.Repeat("z", 32)})},
		{name: "short response", authorization: header(map[string]string{"response": "00"})},
		{name: "wrong method", authorization: header(nil), actualMethod: "RECORD"},
		{name: "wrong actual uri", authorization: header(nil), actualURI: "rtsp://127.0.0.1/other"},
		{name: "unsupported algorithm", authorization: header(map[string]string{"algorithm": "MD5-sess"})},
		{name: "unsupported qop", authorization: header(map[string]string{"qop": "auth"})},
		{name: "qop fields without qop", authorization: header(map[string]string{"cnonce": "abc", "nc": "00000001"})},
		{name: "unknown extension", authorization: header(map[string]string{"unknown": "value"})},
		{name: "duplicate field", authorization: header(nil) + `, Nonce="` + auth.nonce + `"`},
		{name: "missing equals", authorization: `Digest username "AirPlay"`},
		{name: "unterminated quote", authorization: `Digest username="AirPlay`},
		{name: "dangling escape", authorization: `Digest username="AirPlay\`},
		{name: "junk after quote", authorization: `Digest username="AirPlay"junk`},
		{name: "empty segment", authorization: `Digest username="AirPlay",, realm="airplay"`},
		{name: "trailing comma", authorization: header(nil) + ","},
		{name: "header injection", authorization: header(nil) + "\r\nX-Forged: value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actualMethod := tt.actualMethod
			if actualMethod == "" {
				actualMethod = method
			}
			actualURI := tt.actualURI
			if actualURI == "" {
				actualURI = uri
			}
			if auth.authorize(actualMethod, actualURI, tt.authorization) {
				t.Fatalf("invalid Authorization was accepted: %q", tt.authorization)
			}
		})
	}
}

func TestParseReceiverDigestAuthorizationQuotedPairsAndDuplicates(t *testing.T) {
	params, ok := parseReceiverDigestAuthorization(`Digest username="Air\\Play", uri="rtsp://host/a\"b,c", nonce=abc==`)
	if !ok {
		t.Fatal("valid quoted pairs and bare value were rejected")
	}
	if params["username"] != `Air\Play` || params["uri"] != `rtsp://host/a"b,c` || params["nonce"] != "abc==" {
		t.Fatalf("parsed parameters = %#v", params)
	}
	if _, ok := parseReceiverDigestAuthorization(`Digest nonce="a", NONCE="b"`); ok {
		t.Fatal("case-insensitive duplicate was accepted")
	}
}
