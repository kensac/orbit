package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
