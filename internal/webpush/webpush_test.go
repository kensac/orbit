package webpush

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/webpush/webpushtest"
)

// RFC 8291 Appendix A: the worked example, byte for byte.
func TestEncryptRFC8291Example(t *testing.T) {
	asPrivRaw := mustDecode(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	uaPubRaw := mustDecode(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	authSecret := mustDecode(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := mustDecode(t, "DGv6ra1nlYgDCS1FRnbzlw")
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"

	asPriv, err := ecdh.P256().NewPrivateKey(asPrivRaw)
	if err != nil {
		t.Fatal(err)
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaPubRaw)
	if err != nil {
		t.Fatal(err)
	}
	// The example declares rs = 4096, as recordSize does.
	got, err := seal(asPriv, uaPub, authSecret, salt, []byte("When I grow up, I want to be a watermelon"))
	if err != nil {
		t.Fatal(err)
	}
	if b64(got) != want {
		t.Fatalf("ciphertext differs from RFC 8291's example:\n got %s\nwant %s", b64(got), want)
	}
}

// A browser's side of the exchange decrypts what encrypt produced.
func TestEncryptRoundTrip(t *testing.T) {
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := bytes.Repeat([]byte{7}, 16)
	sub := Subscription{P256DH: b64(ua.PublicKey().Bytes()), Auth: b64(auth)}
	payload := []byte(`{"title":"finance has a question","url":"/orbit/c/x"}`)
	body, err := encrypt(sub, payload, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if got := decrypt(t, ua, auth, body); !bytes.Equal(got, payload) {
		t.Fatalf("decrypted %q", got)
	}
	if _, err := encrypt(sub, bytes.Repeat([]byte("x"), MaxPayload+1), rand.Reader); err == nil {
		t.Fatal("an oversized payload was accepted")
	}
}

func TestSendSignsAndReportsGone(t *testing.T) {
	pub, priv, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParseKeys(pub, priv, "mailto:ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseKeys(pub, priv, "ops@example.com"); err == nil {
		t.Fatal("a subject without a scheme was accepted")
	}
	otherPub, _, _ := GenerateKeys()
	if _, err := ParseKeys(otherPub, priv, "mailto:a@b"); err == nil {
		t.Fatal("a mismatched public key was accepted")
	}

	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := bytes.Repeat([]byte{1}, 16)
	status := http.StatusCreated
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	sub := Subscription{Endpoint: srv.URL + "/push/abc", P256DH: b64(ua.PublicKey().Bytes()), Auth: b64(auth)}
	err = keys.Send(context.Background(), srv.Client(), sub, Message{Payload: []byte("hi"), TTL: time.Hour, Topic: Topic("action:1"), Urgency: "high"})
	if err != nil {
		t.Fatal(err)
	}
	for h, want := range map[string]string{"Content-Encoding": "aes128gcm", "TTL": "3600", "Urgency": "high", "Topic": Topic("action:1")} {
		if got.Header.Get(h) != want {
			t.Errorf("%s = %q, want %q", h, got.Header.Get(h), want)
		}
	}
	if len(Topic("anything at all")) != 32 {
		t.Error("Topic is not 32 characters")
	}
	if string(decrypt(t, ua, auth, gotBody)) != "hi" {
		t.Error("the push service received something the browser cannot decrypt")
	}
	verifyVAPID(t, got.Header.Get("Authorization"), keys, srv.URL)

	status = http.StatusGone
	if err := keys.Send(context.Background(), srv.Client(), sub, Message{Payload: []byte("hi")}); !errors.Is(err, ErrGone) {
		t.Fatalf("410: %v, want ErrGone", err)
	}
	status = http.StatusTooManyRequests
	if err := keys.Send(context.Background(), srv.Client(), sub, Message{Payload: []byte("hi")}); err == nil || errors.Is(err, ErrGone) {
		t.Fatalf("429: %v, want a plain error", err)
	}
}

func verifyVAPID(t *testing.T, header string, keys *Keys, origin string) {
	t.Helper()
	rest, ok := strings.CutPrefix(header, "vapid t=")
	if !ok {
		t.Fatalf("Authorization %q", header)
	}
	jwt, k, ok := strings.Cut(rest, ", k=")
	if !ok || k != keys.Public {
		t.Fatalf("Authorization %q does not carry the public key", header)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT %q", jwt)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(mustDecode(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != origin || claims.Sub != keys.Subject || claims.Exp <= time.Now().Unix() || claims.Exp > time.Now().Add(24*time.Hour).Unix() {
		t.Fatalf("claims %+v", claims)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(keys.private.Curve, mustDecode(t, keys.Public))
	if err != nil {
		t.Fatal(err)
	}
	sig := mustDecode(t, parts[2])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("VAPID signature does not verify")
	}
}

// decrypt is the user agent's half of RFC 8291.
func decrypt(t *testing.T, ua *ecdh.PrivateKey, auth, body []byte) []byte {
	t.Helper()
	if rs := binary.BigEndian.Uint32(body[16:20]); rs != recordSize {
		t.Fatalf("rs %d", rs)
	}
	plain, err := webpushtest.Decrypt(ua, auth, body)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decode(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
