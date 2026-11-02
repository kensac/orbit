package connectors

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MinKeyLen is the shortest ORBIT_CONNECTOR_KEY accepted.
const MinKeyLen = 32

// Sealer encrypts what orbit keeps for a connector (its tokens, at rest in the database; a sign-in's state, in a URL) under a key from ORBIT_CONNECTOR_KEY, so a database dump or backup holds no usable token. Every instance with the same key opens what another sealed.
type Sealer struct{ aead cipher.AEAD }

// NewSealer derives the sealing key from secret.
func NewSealer(secret string) (*Sealer, error) {
	if len(secret) < MinKeyLen {
		return nil, fmt.Errorf("the connector key must be at least %d characters", MinKeyLen)
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("orbit connector tokens"))
	b, err := aes.NewCipher(m.Sum(nil))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// envelope is a sealed value with its kind (a state is never opened as a token) and expiry (0: none).
type envelope struct {
	Kind string          `json:"k"`
	Exp  int64           `json:"e,omitempty"`
	Body json.RawMessage `json:"b"`
}

// Seal encrypts v as kind, valid for ttl (0: no expiry).
func (s *Sealer) Seal(kind string, v any, ttl time.Duration) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	env := envelope{Kind: kind, Body: body}
	if ttl > 0 {
		env.Exp = time.Now().Add(ttl).Unix()
	}
	plain, _ := json.Marshal(env)
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, []byte(kind)), nil
}

// Open decrypts b, sealed as kind, into v.
func (s *Sealer) Open(kind string, b []byte, v any) error {
	n := s.aead.NonceSize()
	if len(b) < n {
		return errors.New("sealed value too short")
	}
	plain, err := s.aead.Open(nil, b[:n], b[n:], []byte(kind))
	if err != nil {
		return errors.New("sealed value does not open with this key")
	}
	var env envelope
	if err := json.Unmarshal(plain, &env); err != nil || env.Kind != kind {
		return errors.New("sealed value of another kind")
	}
	if env.Exp != 0 && time.Now().Unix() > env.Exp {
		return errors.New("sealed value expired")
	}
	return json.Unmarshal(env.Body, v)
}

// SealString is Seal as URL-safe text.
func (s *Sealer) SealString(kind string, v any, ttl time.Duration) (string, error) {
	b, err := s.Seal(kind, v, ttl)
	return base64.RawURLEncoding.EncodeToString(b), err
}

// OpenString opens what SealString sealed.
func (s *Sealer) OpenString(kind, text string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		return errors.New("sealed value is not base64url")
	}
	return s.Open(kind, b, v)
}
