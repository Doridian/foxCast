package sender

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	receiverDigestRealm     = "airplay"
	receiverDigestNonceSize = 32
)

// receiverDigestAuth implements the RFC 2069 Digest authentication profile
// used by AirPlay receivers. A receiver keeps one instance for its lifetime so
// the challenge remains stable across the initial 401 and later requests.
type receiverDigestAuth struct {
	password string
	realm    string
	nonce    string
}

func newReceiverDigestAuth(password string) (*receiverDigestAuth, error) {
	auth := &receiverDigestAuth{password: password}
	if password == "" {
		return auth, nil
	}

	nonce := make([]byte, receiverDigestNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate receiver digest nonce: %w", err)
	}
	auth.realm = receiverDigestRealm
	auth.nonce = base64.RawStdEncoding.EncodeToString(nonce)
	return auth, nil
}

func (a *receiverDigestAuth) enabled() bool {
	return a != nil && a.password != ""
}

// challengeHeader returns the value for WWW-Authenticate. An empty value means
// receiver authentication is disabled.
func (a *receiverDigestAuth) challengeHeader() string {
	if !a.enabled() {
		return ""
	}
	return fmt.Sprintf(`Digest realm="%s", nonce="%s"`, a.realm, a.nonce)
}

// authorize validates an Authorization header for the exact request method and
// URI. A disabled authenticator permits the request without inspecting the
// header, which keeps the receiver call site to one conditional.
func (a *receiverDigestAuth) authorize(method, uri, authorization string) bool {
	if !a.enabled() {
		return true
	}

	params, ok := parseReceiverDigestAuthorization(authorization)
	if !ok {
		return false
	}

	for name := range params {
		switch name {
		case "username", "realm", "nonce", "uri", "response", "algorithm":
		case "qop", "cnonce", "nc":
			return false
		default:
			// This receiver advertises only the RFC 2069 profile. Rejecting
			// unknown extensions avoids silently interpreting an ambiguous
			// authorization value under different rules than the sender.
			return false
		}
	}

	username, hasUsername := params["username"]
	realm, hasRealm := params["realm"]
	nonce, hasNonce := params["nonce"]
	digestURI, hasURI := params["uri"]
	response, hasResponse := params["response"]
	if !hasUsername || !hasRealm || !hasNonce || !hasURI || !hasResponse ||
		username == "" || realm == "" || nonce == "" || digestURI == "" || response == "" {
		return false
	}
	if username != DigestUsername || realm != a.realm || nonce != a.nonce || digestURI != uri {
		return false
	}
	if algorithm, present := params["algorithm"]; present && !strings.EqualFold(algorithm, "MD5") {
		return false
	}

	provided, err := hex.DecodeString(response)
	if err != nil || len(provided) != 16 {
		return false
	}
	expected, err := hex.DecodeString(digestResponse(DigestUsername, a.realm, a.password, a.nonce, method, uri))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(provided, expected) == 1
}

// parseReceiverDigestAuthorization parses a strict Digest credentials value.
// Parameter names are case-insensitive, quoted strings support quoted-pair
// escapes, and bare values extend to the next comma. Duplicate or malformed
// parameters make the complete header invalid.
func parseReceiverDigestAuthorization(value string) (map[string]string, bool) {
	if strings.ContainsAny(value, "\r\n") {
		return nil, false
	}

	value = strings.TrimSpace(value)
	const scheme = "Digest"
	if len(value) <= len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) || !isReceiverDigestSpace(value[len(scheme)]) {
		return nil, false
	}
	value = strings.TrimSpace(value[len(scheme):])
	if value == "" {
		return nil, false
	}

	params := make(map[string]string)
	for offset := 0; offset < len(value); {
		for offset < len(value) && isReceiverDigestSpace(value[offset]) {
			offset++
		}
		keyStart := offset
		for offset < len(value) && isReceiverDigestTokenChar(value[offset]) {
			offset++
		}
		if keyStart == offset {
			return nil, false
		}
		key := strings.ToLower(value[keyStart:offset])
		for offset < len(value) && isReceiverDigestSpace(value[offset]) {
			offset++
		}
		if offset >= len(value) || value[offset] != '=' {
			return nil, false
		}
		offset++
		for offset < len(value) && isReceiverDigestSpace(value[offset]) {
			offset++
		}
		if offset >= len(value) {
			return nil, false
		}

		var parsed string
		if value[offset] == '"' {
			offset++
			var quoted strings.Builder
			closed := false
			for offset < len(value) {
				ch := value[offset]
				offset++
				switch ch {
				case '"':
					closed = true
				case '\\':
					if offset >= len(value) || isReceiverDigestControl(value[offset]) {
						return nil, false
					}
					quoted.WriteByte(value[offset])
					offset++
				default:
					if isReceiverDigestControl(ch) {
						return nil, false
					}
					quoted.WriteByte(ch)
				}
				if closed {
					break
				}
			}
			if !closed {
				return nil, false
			}
			parsed = quoted.String()
			for offset < len(value) && isReceiverDigestSpace(value[offset]) {
				offset++
			}
			if offset < len(value) && value[offset] != ',' {
				return nil, false
			}
		} else {
			valueStart := offset
			for offset < len(value) && value[offset] != ',' {
				if value[offset] == '"' || isReceiverDigestControl(value[offset]) {
					return nil, false
				}
				offset++
			}
			parsed = strings.TrimSpace(value[valueStart:offset])
			if parsed == "" || strings.IndexFunc(parsed, func(r rune) bool {
				return r == ' ' || r == '\t'
			}) >= 0 {
				return nil, false
			}
		}

		if _, duplicate := params[key]; duplicate {
			return nil, false
		}
		params[key] = parsed

		if offset == len(value) {
			break
		}
		// A comma must separate two complete parameters; empty segments and a
		// trailing comma are malformed rather than silently ignored.
		offset++
		lookahead := offset
		for lookahead < len(value) && isReceiverDigestSpace(value[lookahead]) {
			lookahead++
		}
		if lookahead == len(value) || value[lookahead] == ',' {
			return nil, false
		}
	}

	return params, true
}

func isReceiverDigestSpace(ch byte) bool {
	return ch == ' ' || ch == '\t'
}

func isReceiverDigestControl(ch byte) bool {
	return ch < 0x20 || ch == 0x7f
}

func isReceiverDigestTokenChar(ch byte) bool {
	if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' {
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", rune(ch))
}
