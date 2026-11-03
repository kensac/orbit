package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// GitHubSignature is how GitHub signs body under secret (X-Hub-Signature-256).
func gitHubSignature(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func standardSignature(key []byte, id string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + strconv.FormatInt(ts, 10) + "."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func TestGitHubSignature(t *testing.T) {
	secret, body := []byte("It's a Secret to Everybody"), []byte("Hello, World!")
	// GitHub's documented test vector (docs: Validating webhook deliveries).
	const want = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	if got := gitHubSignature(secret, body); got != want {
		t.Fatalf("signature %s, want GitHub's %s", got, want)
	}
	h := http.Header{}
	h.Set("X-Hub-Signature-256", want)
	h.Set("X-GitHub-Delivery", "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	h.Set("X-GitHub-Event", "issues")
	d, err := Verify("github", secret, h, body, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "72d3162e-cc78-11e3-81ab-4c9367dc0958" || d.Event != "issues" || d.SentAt != nil {
		t.Fatalf("delivery %+v", d)
	}
	if _, err := Verify("github", secret, h, []byte("Hello, World?"), time.Now(), time.Minute); !errors.Is(err, ErrSignature) {
		t.Fatalf("a changed body: %v, want ErrSignature", err)
	}
	if _, err := Verify("github", []byte("other"), h, body, time.Now(), time.Minute); !errors.Is(err, ErrSignature) {
		t.Fatalf("another secret: %v, want ErrSignature", err)
	}
	h.Set("X-Hub-Signature-256", "sha1=abc")
	if _, err := Verify("github", secret, h, body, time.Now(), time.Minute); !errors.Is(err, ErrMalformed) {
		t.Fatalf("a sha1 signature: %v, want ErrMalformed", err)
	}
	h.Set("X-Hub-Signature-256", want)
	h.Del("X-GitHub-Delivery")
	if _, err := Verify("github", secret, h, body, time.Now(), time.Minute); !errors.Is(err, ErrMalformed) {
		t.Fatalf("no delivery id: %v, want ErrMalformed", err)
	}
}

func TestStandardSignature(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secret := []byte("whsec_" + base64.StdEncoding.EncodeToString(key))
	body := []byte(`{"type":"invoice.paid"}`)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	header := func(ts int64, sig string) http.Header {
		h := http.Header{}
		h.Set("webhook-id", "msg_1")
		h.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
		h.Set("webhook-signature", sig)
		return h
	}
	ts := now.Add(-time.Minute).Unix()
	d, err := Verify("standard", secret, header(ts, "v1,bm90IGl0 "+standardSignature(key, "msg_1", ts, body)), body, now, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "msg_1" || d.SentAt == nil || d.SentAt.Unix() != ts {
		t.Fatalf("delivery %+v", d)
	}
	// A raw secret (no whsec_) is used as given.
	if _, err := Verify("standard", key, header(ts, standardSignature(key, "msg_1", ts, body)), body, now, 5*time.Minute); err != nil {
		t.Fatalf("raw secret: %v", err)
	}
	old := now.Add(-6 * time.Minute).Unix()
	if _, err := Verify("standard", secret, header(old, standardSignature(key, "msg_1", old, body)), body, now, 5*time.Minute); !errors.Is(err, ErrStale) {
		t.Fatalf("a replay six minutes old: %v, want ErrStale", err)
	}
	future := now.Add(6 * time.Minute).Unix()
	if _, err := Verify("standard", secret, header(future, standardSignature(key, "msg_1", future, body)), body, now, 5*time.Minute); !errors.Is(err, ErrStale) {
		t.Fatalf("six minutes ahead: %v, want ErrStale", err)
	}
	// The timestamp is signed: moving it breaks the signature.
	if _, err := Verify("standard", secret, header(ts+1, standardSignature(key, "msg_1", ts, body)), body, now, 5*time.Minute); !errors.Is(err, ErrSignature) {
		t.Fatalf("a moved timestamp: %v, want ErrSignature", err)
	}
	if _, err := Verify("standard", secret, header(ts, "v2,"+standardSignature(key, "msg_1", ts, body)[3:]), body, now, 5*time.Minute); !errors.Is(err, ErrSignature) {
		t.Fatalf("an unknown version: %v, want ErrSignature", err)
	}
	h := header(ts, "v1,x")
	h.Set("webhook-timestamp", "yesterday")
	if _, err := Verify("standard", secret, h, body, now, 5*time.Minute); !errors.Is(err, ErrMalformed) {
		t.Fatalf("a timestamp that is not a number: %v, want ErrMalformed", err)
	}
}
