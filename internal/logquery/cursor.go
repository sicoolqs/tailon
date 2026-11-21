package logquery

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

const cursorVersion = 1

// CursorFile pins the identity and size of a file at the time a query started.
// A cursor cannot be resumed if the file was rotated, truncated or replaced.
type CursorFile struct {
	ID       string `json:"id"`
	Size     int64  `json:"size"`
	Identity string `json:"identity,omitempty"`
}

// cursorPayload is the signed, opaque state stored in a continuation cursor.
// It is intentionally small: the server keeps no per-query session state.
type cursorPayload struct {
	Version int    `json:"v"`
	Tool    string `json:"tool"`
	Query   string `json:"query"`
	Expiry  int64  `json:"expires"`

	Files  []CursorFile `json:"files,omitempty"`
	Index  int          `json:"index,omitempty"`
	Offset int64        `json:"offset,omitempty"`
	Dir    string       `json:"dir,omitempty"`

	AfterMtime int64  `json:"after_mtime,omitempty"`
	AfterName  string `json:"after_name,omitempty"`
	AfterID    string `json:"after_id,omitempty"`

	SnapshotSize int64 `json:"snapshot_size,omitempty"`
	Returned     int   `json:"returned,omitempty"`
}

// CursorCodec signs and verifies cursors with a per-process random key. This
// protects cursor integrity; it is not an authentication mechanism.
type CursorCodec struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// NewCursorCodec creates a codec with a random process-local key.
func NewCursorCodec(ttl time.Duration) *CursorCodec {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		sum := sha256.Sum256([]byte(time.Now().String()))
		key = sum[:]
	}
	return &CursorCodec{key: key, ttl: ttl, now: time.Now}
}

// Encode signs a cursor payload.
func (c *CursorCodec) Encode(p cursorPayload) string {
	p.Version = cursorVersion
	if p.Expiry == 0 {
		p.Expiry = c.now().Add(c.ttl).UnixNano()
	}
	body, _ := json.Marshal(p)
	bodyB64 := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(bodyB64))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return bodyB64 + "." + sig
}

// Decode verifies and parses a cursor. The caller must still check the tool
// name and query digest against the current request.
func (c *CursorCodec) Decode(token string) (*cursorPayload, error) {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, NewError(CodeInvalidCursor, "the cursor is malformed").
			WithHint(false, "Restart the query without a cursor.")
	}
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(parts[0]))
	expected := mac.Sum(nil)
	actual, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || subtle.ConstantTimeCompare(expected, actual) != 1 {
		return nil, NewError(CodeInvalidCursor, "the cursor signature is invalid").
			WithHint(false, "Restart the query without a cursor.")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, NewError(CodeInvalidCursor, "the cursor payload is invalid")
	}
	var p cursorPayload
	if err := json.Unmarshal(body, &p); err != nil || p.Version != cursorVersion {
		return nil, NewError(CodeInvalidCursor, "the cursor payload is not supported").
			WithHint(false, "Restart the query without a cursor.")
	}
	if c.now().UnixNano() > p.Expiry {
		return nil, NewError(CodeCursorExpired, "the continuation cursor has expired").
			WithHint(false, "Restart the query without a cursor.")
	}
	return &p, nil
}

// queryDigest binds a cursor to the parameters that produced it.
func queryDigest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
