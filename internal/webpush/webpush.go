// Package webpush sends Web Push messages straight to a browser's push
// service: the payload encrypted to the subscription (RFC 8291, aes128gcm)
// and the request signed with orbit's VAPID key (RFC 8292). No relay sits
// between orbit and the push service, and the push service sees only
// ciphertext.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Keys is a VAPID key pair and the contact the push service may reach the
// sender at (a mailto: or https: URL).
type Keys struct {
	private *ecdsa.PrivateKey
	// Public is the uncompressed P-256 point, base64url without padding:
	// what a browser passes to pushManager.subscribe as applicationServerKey.
	Public  string
	Subject string
}

// ParseKeys reads a VAPID key pair as the web-push tools print it: the
// public key as a base64url uncompressed point, the private key as the
// base64url 32-byte scalar.
func ParseKeys(public, private, subject string) (*Keys, error) {
	d, err := decode(private)
	if err != nil || len(d) != 32 {
		return nil, errors.New("VAPID private key: want base64url of 32 bytes")
	}
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
	if err != nil {
		return nil, fmt.Errorf("VAPID private key: %w", err)
	}
	pub, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	if p, err := decode(public); err != nil || !bytes.Equal(p, pub) {
		return nil, errors.New("VAPID public key does not match the private key")
	}
	if !strings.HasPrefix(subject, "mailto:") && !strings.HasPrefix(subject, "https://") {
		return nil, errors.New("VAPID subject: want a mailto: or https: URL")
	}
	return &Keys{private: priv, Public: b64(pub), Subject: subject}, nil
}

// GenerateKeys makes a new VAPID pair, printed by `orbit vapid`.
func GenerateKeys() (public, private string, err error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	pub, err := k.PublicKey.Bytes()
	if err != nil {
		return "", "", err
	}
	d, err := k.Bytes()
	if err != nil {
		return "", "", err
	}
	return b64(pub), b64(d), nil
}

// Subscription is the part of a browser's PushSubscription a sender needs.
type Subscription struct {
	Endpoint string
	P256DH   string // the browser's public key, base64url
	Auth     string // the browser's auth secret, base64url
}

// Message is one push.
type Message struct {
	Payload []byte
	// TTL is how long the push service keeps it for an offline device.
	TTL time.Duration
	// Topic replaces an undelivered message with the same topic at the push
	// service (at most 32 base64url characters; Topic() makes one).
	Topic string
	// Urgency is very-low, low, normal or high.
	Urgency string
}

// ErrGone means the push service no longer knows the subscription (404 or
// 410): the browser dropped it, so stop sending to it.
var ErrGone = errors.New("subscription is gone")

// Topic turns any identity into a valid Topic header value.
func Topic(identity string) string {
	h := sha256.Sum256([]byte(identity))
	return b64(h[:])[:32]
}

// Send delivers one message. It returns ErrGone (wrapped) for a
// subscription the push service has dropped.
func (k *Keys) Send(ctx context.Context, client *http.Client, s Subscription, m Message) error {
	body, err := encrypt(s, m.Payload, rand.Reader)
	if err != nil {
		return err
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("endpoint %q is not a URL", s.Endpoint)
	}
	jwt, err := k.token(u.Scheme+"://"+u.Host, time.Now().Add(12*time.Hour))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(int(m.TTL.Seconds())))
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+k.Public)
	if m.Topic != "" {
		req.Header.Set("Topic", m.Topic)
	}
	if m.Urgency != "" {
		req.Header.Set("Urgency", m.Urgency)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return fmt.Errorf("%s: %w", u.Host, ErrGone)
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("%s: %s: %s", u.Host, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// token is the VAPID JWT (ES256) for one push service origin.
func (k *Keys) token(aud string, exp time.Time) (string, error) {
	head := b64([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{"aud": aud, "exp": exp.Unix(), "sub": k.Subject})
	if err != nil {
		return "", err
	}
	signing := head + "." + b64(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.private, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64(sig), nil
}

// recordSize is the aes128gcm record size orbit declares; a payload must
// fit one record (rs minus the 16-byte tag and the delimiter), and push
// services accept at most 4096 bytes of body anyway.
const recordSize = 4096

// MaxPayload is the largest payload one record carries.
const MaxPayload = recordSize - 16 - 1 - 86 // tag, delimiter, header

// encrypt is RFC 8291 section 3 with one record and no padding.
func encrypt(s Subscription, plaintext []byte, random io.Reader) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, fmt.Errorf("payload is %d bytes, at most %d fit", len(plaintext), MaxPayload)
	}
	uaRaw, err := decode(s.P256DH)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaRaw)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	authSecret, err := decode(s.Auth)
	if err != nil || len(authSecret) < 16 {
		return nil, errors.New("auth: want base64url of 16 bytes")
	}
	asPriv, err := ecdh.P256().GenerateKey(random)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(random, salt); err != nil {
		return nil, err
	}
	return seal(asPriv, uaPub, authSecret, salt, plaintext)
}

func seal(asPriv *ecdh.PrivateKey, uaPub *ecdh.PublicKey, authSecret, salt, plaintext []byte) ([]byte, error) {
	shared, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPub := asPriv.PublicKey().Bytes()
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPub.Bytes()...), asPub...)
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, 86)
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, 65) // idlen: an uncompressed P-256 point is 65 bytes
	header = append(header, asPub...)
	// 0x02 marks the last (only) record.
	return gcm.Seal(header, nonce, append(append([]byte{}, plaintext...), 2), nil), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// decode accepts base64url with or without padding, and standard base64
// (some browsers' toJSON and older tools differ).
func decode(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	s = strings.NewReplacer("+", "-", "/", "_").Replace(s)
	return base64.RawURLEncoding.DecodeString(s)
}
