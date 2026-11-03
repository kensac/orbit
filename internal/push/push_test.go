package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/secret"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
	"github.com/kensac/orbit/internal/webpush"
)

var ctx = context.Background()

func ptr(s string) *string { return &s }

func TestSummarizeCarriesNoContent(t *testing.T) {
	one := store.Attention{Key: "action:x", Kind: store.AttentionApproval, Agent: "finance", ConversationID: "c1",
		ActionID: ptr("x"), Title: "finance wants to webhook:pay", Detail: "pay $2,700 rent"}
	p := summarize("/orbit", []store.Attention{one})
	if p.Title != one.Title || p.URL != "/orbit/c/c1#action-x" || p.Tag != "action:x" || p.Count != 1 {
		t.Fatalf("one = %+v", p)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "2,700") {
		t.Fatalf("a payload carried the detail: %s", b)
	}
	q := store.Attention{Key: "question:m", Kind: store.AttentionQuestion, Agent: "ops", ConversationID: "c2", MessageID: ptr("m"), Title: "ops has a question"}
	f := store.Attention{Key: "failure:r", Kind: store.AttentionFailure, Agent: "finance", ConversationID: "c1", Title: "finance run failed"}
	p = summarize("/orbit", []store.Attention{one, q, f})
	if p.Title != "3 things need you: finance, ops" || p.URL != "/orbit/inbox" || p.Tag != "orbit-inbox" || p.Count != 3 {
		t.Fatalf("summary = %+v", p)
	}
	if Link("/orbit", q) != "/orbit/c/c2#message-m" || Link("/orbit", f) != "/orbit/c/c1" {
		t.Fatalf("links %s %s", Link("/orbit", q), Link("/orbit", f))
	}
	pr := store.Attention{Key: "approval:p", Kind: store.AttentionPermission, Agent: "orbit", ConversationID: "c3", ApprovalID: ptr("p"),
		Title: "orbit needs permission", Detail: "Bash"}
	if p := summarize("/orbit", []store.Attention{pr}); p.URL != "/orbit/c/c3#approval-p" || p.Title != "orbit needs permission" || p.Tag != "approval:p" {
		t.Fatalf("permission = %+v", p)
	}
}

func TestAllowedEndpoint(t *testing.T) {
	for _, ok := range []string{
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://web.push.apple.com/QH",
		"https://wns2-bl2p.notify.windows.com/w/?token=x",
		"https://updates.push.services.mozilla.com/wpush/v2/x",
	} {
		if err := AllowedEndpoint(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://fcm.googleapis.com/fcm/send/abc",
		"https://evilfcm.googleapis.com.example/x",
		"https://169.254.169.254/latest",
		"https://user@fcm.googleapis.com/x",
		"not a url",
	} {
		if err := AllowedEndpoint(bad); err == nil {
			t.Errorf("%s was allowed", bad)
		}
	}
}

func TestDeliverSendsOncePerDeviceAndRevokesGone(t *testing.T) {
	s := testdb.New(t)
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"a": {SHA: "sha-a", JSON: []byte(`{"name":"a"}`), Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	for _, path := range []string{"/phone", "/gone"} {
		k, _ := ecdh.P256().GenerateKey(rand.Reader)
		if _, err := s.SavePushSubscription(ctx, store.PushSubscription{Endpoint: srv.URL + path, Author: "kanishk",
			P256DH: base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, _ := webpush.GenerateKeys()
	keys, _ := webpush.ParseKeys(pub, priv, "mailto:a@b.c")
	p := &Sender{Store: s, Keys: keys, Client: srv.Client(), LinkBase: "/orbit", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	id, err := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: "sha-a", Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.ClaimRun(ctx, "w")
	if err := s.FinishRun(ctx, id, store.RunResult{Attempt: 1, Status: "failed", Error: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Deliver(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Deliver(ctx); err != nil {
		t.Fatal(err)
	}
	if hits["/phone"] != 1 || hits["/gone"] != 1 {
		t.Fatalf("hits = %v, want one push per device", hits)
	}
	subs, _ := s.ListPushSubscriptions(ctx, "")
	if len(subs) != 1 || !strings.HasSuffix(subs[0].Endpoint, "/phone") || subs[0].LastSentAt == nil {
		t.Fatalf("active = %+v, want only the phone, sent", subs)
	}
	if n, err := p.Test(ctx, "kanishk"); err != nil || n != 1 {
		t.Fatalf("test push = %d %v", n, err)
	}
}

func TestPresenceKeepsAPushOffTheDevicesOfWhoeverIsLooking(t *testing.T) {
	s := testdb.New(t)
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"a": {SHA: "sha-a", JSON: []byte(`{"name":"a"}`), Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	got := map[string]int{} // pushes per device path
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path]++
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	for _, d := range []struct{ path, author string }{{"/kanishk-phone", "kanishk"}, {"/kanishk-laptop", "kanishk"}, {"/other", "other"}} {
		k, _ := ecdh.P256().GenerateKey(rand.Reader)
		if _, err := s.SavePushSubscription(ctx, store.PushSubscription{Endpoint: srv.URL + d.path, Author: d.author,
			P256DH: base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, _ := webpush.GenerateKeys()
	keys, _ := webpush.ParseKeys(pub, priv, "mailto:a@b.c")
	p := &Sender{Store: s, Keys: keys, Client: srv.Client(), LinkBase: "/orbit", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	fail := func() string {
		t.Helper()
		id, err := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: "sha-a", Trigger: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimRun(ctx, "w"); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRun(ctx, id, store.RunResult{Attempt: 1, Status: "failed", Error: "x"}); err != nil {
			t.Fatal(err)
		}
		var conv string
		if err := s.Pool.QueryRow(ctx, `SELECT conversation_id::text FROM runs WHERE id = $1`, id).Scan(&conv); err != nil {
			t.Fatal(err)
		}
		return conv
	}
	deliver := func() map[string]int {
		t.Helper()
		mu.Lock()
		got = map[string]int{}
		mu.Unlock()
		// The gap is fleet-wide; each case starts clear of it.
		if _, err := s.Pool.Exec(ctx, `UPDATE push_state SET last_push_at = now() - interval '1 hour'`); err != nil {
			t.Fatal(err)
		}
		if err := p.Deliver(ctx); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(got)
	}

	conv := fail()
	if err := s.SetPresence(ctx, "kanishk", &conv); err != nil {
		t.Fatal(err)
	}
	if hits := deliver(); hits["/kanishk-phone"] != 0 || hits["/kanishk-laptop"] != 0 || hits["/other"] != 1 {
		t.Fatalf("hits = %v: kanishk is looking at the conversation, other is not", hits)
	}

	// Another conversation's push still reaches the person looking elsewhere.
	other := "00000000-0000-0000-0000-000000000001"
	if err := s.SetPresence(ctx, "kanishk", &other); err != nil {
		t.Fatal(err)
	}
	fail()
	if hits := deliver(); hits["/kanishk-phone"] != 1 || hits["/kanishk-laptop"] != 1 || hits["/other"] != 1 {
		t.Fatalf("hits = %v: presence elsewhere silences nothing here", hits)
	}

	// A stale heartbeat never silences.
	if err := s.SetPresence(ctx, "kanishk", &conv); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE presence SET seen_at = now() - interval '61 seconds'`); err != nil {
		t.Fatal(err)
	}
	fail()
	if hits := deliver(); hits["/kanishk-phone"] != 1 || hits["/kanishk-laptop"] != 1 {
		t.Fatalf("hits = %v: a heartbeat older than %s silences nothing", hits, Presence)
	}

	// Null (the app shows no conversation) silences nothing either.
	if err := s.SetPresence(ctx, "kanishk", nil); err != nil {
		t.Fatal(err)
	}
	fail()
	if hits := deliver(); hits["/kanishk-phone"] != 1 {
		t.Fatalf("hits = %v: no conversation shown", hits)
	}
}

func TestActionTokens(t *testing.T) {
	key := secret.Key("root")
	now := time.Unix(1_800_000_000, 0)
	ok := ActionToken{Kind: "approval", ID: "x", Decision: DecideDeny, Person: "kanishk", Nonce: "n", Exp: now.Add(time.Minute).Unix()}
	if got, err := ParseAction(key, SignAction(key, ok), now); err != nil || got != ok {
		t.Fatalf("round trip = %+v %v", got, err)
	}
	if _, err := ParseAction(key, SignAction(key, ok), now.Add(time.Minute)); !errors.Is(err, ErrActionExpired) {
		t.Fatalf("expired = %v", err)
	}
	if _, err := ParseAction(secret.Key("other"), SignAction(key, ok), now); !errors.Is(err, ErrActionLink) {
		t.Fatalf("another root = %v", err)
	}
	if _, err := ParseAction(nil, SignAction(key, ok), now); !errors.Is(err, ErrActionLink) {
		t.Fatalf("no key = %v", err)
	}
	for _, bad := range []ActionToken{
		{Kind: "action", ID: "x", Decision: DecideDeny, Person: "p", Nonce: "n", Exp: ok.Exp},     // deny is for permission requests
		{Kind: "approval", ID: "x", Decision: DecideReject, Person: "p", Nonce: "n", Exp: ok.Exp}, // reject is for actions
		{Kind: "ticket", ID: "x", Decision: DecideApprove, Person: "p", Nonce: "n", Exp: ok.Exp},
		{Kind: "action", ID: "x", Decision: DecideApprove, Nonce: "n", Exp: ok.Exp},
	} {
		if _, err := ParseAction(key, SignAction(key, bad), now); !errors.Is(err, ErrActionLink) {
			t.Errorf("%+v = %v", bad, err)
		}
	}
	tok := SignAction(key, ok)
	body, mac, _ := strings.Cut(tok, ".")
	forged := SignAction(key, ActionToken{Kind: "approval", ID: "y", Decision: DecideApprove, Person: "kanishk", Nonce: "n", Exp: ok.Exp})
	fbody, _, _ := strings.Cut(forged, ".")
	if _, err := ParseAction(key, fbody+"."+mac, now); !errors.Is(err, ErrActionLink) || body == fbody {
		t.Fatalf("a swapped body = %v", err)
	}
}

func TestOnlyApprovalsAndPermissionRequestsGetButtons(t *testing.T) {
	s := &Sender{Secret: secret.Key("root"), PublicURL: func() string { return "https://orbit.example/" }}
	now := time.Now()
	ap := store.Attention{Kind: store.AttentionApproval, ActionID: ptr("a1")}
	if got := s.actions(ap, "kanishk", now); len(got) != 3 || !strings.HasPrefix(got[0].URL, "https://orbit.example"+ActionPath+"?t=") {
		t.Fatalf("approval = %+v", got)
	}
	for _, a := range []store.Attention{
		{Kind: store.AttentionQuestion, MessageID: ptr("m")},
		{Kind: store.AttentionFailure},
		{Kind: store.AttentionDecision, ActionID: ptr("d")},
	} {
		if got := s.actions(a, "kanishk", now); got != nil {
			t.Errorf("%s got buttons %+v", a.Kind, got)
		}
	}
	if got := (&Sender{Secret: secret.Key("root")}).actions(ap, "kanishk", now); got != nil {
		t.Errorf("without a public URL: %+v", got)
	}
	if !silentRevokes("https://web.push.apple.com/QH") || silentRevokes("https://fcm.googleapis.com/fcm/send/x") {
		t.Error("only Apple's push service is skipped for close pushes")
	}
}

func TestPushActionNonceFailureFailsClosed(t *testing.T) {
	s := &Sender{Secret: secret.Key("root"), PublicURL: func() string { return "https://orbit.example" }}
	a := store.Attention{Kind: store.AttentionApproval, ActionID: ptr("a1")}
	if got := s.actionsWithNonce(a, "kanishk", time.Now(), strings.NewReader("")); got != nil {
		t.Fatalf("nonce read failure returned action buttons: %+v", got)
	}
}
