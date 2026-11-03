package api_test

import (
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
	"time"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/push"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/webpush"
	"github.com/kensac/orbit/internal/webpush/webpushtest"
)

// device is a push subscription whose pushes the test reads.
type device struct {
	mu     sync.Mutex
	key    *ecdh.PrivateKey
	auth   []byte
	srv    *httptest.Server
	pushes []push.Payload
}

func newDevice(t *testing.T) *device {
	d := &device{auth: make([]byte, 16)}
	d.key, _ = ecdh.P256().GenerateKey(rand.Reader)
	_, _ = rand.Read(d.auth)
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		plain, err := webpushtest.Decrypt(d.key, d.auth, body)
		if err != nil {
			t.Error(err)
		}
		var p push.Payload
		_ = json.Unmarshal(plain, &p)
		if r.Header.Get("Topic") != webpush.Topic(p.Tag) {
			t.Errorf("topic %q for tag %q", r.Header.Get("Topic"), p.Tag)
		}
		d.mu.Lock()
		d.pushes = append(d.pushes, p)
		d.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *device) take() []push.Payload {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.pushes
	d.pushes = nil
	return out
}

func post(t *testing.T, u string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(u, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("no CORS header on %s", u)
	}
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func TestPushActionsDecideOnceWithoutASessionAndCloseElsewhere(t *testing.T) {
	pub, priv, _ := webpush.GenerateKeys()
	keys, _ := webpush.ParseKeys(pub, priv, "mailto:a@b.c")
	var sender *push.Sender
	v := newEnv(t, func(a *api.Server, _ *http.Server) {
		sender = &push.Sender{Store: a.Store, Keys: keys, Client: http.DefaultClient, LinkBase: "/orbit", Secret: []byte(token),
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		a.Push = sender
	})
	sender.PublicURL = func() string { return v.srv.URL + "/" }
	phone := newDevice(t)
	if _, err := v.s.SavePushSubscription(ctx, store.PushSubscription{Endpoint: phone.srv.URL + "/p", Author: "kanishk",
		P256DH: base64.RawURLEncoding.EncodeToString(phone.key.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(phone.auth)}); err != nil {
		t.Fatal(err)
	}
	deliver := func() []push.Payload {
		t.Helper()
		if _, err := v.s.Pool.Exec(ctx, `UPDATE push_state SET last_push_at = now() - interval '1 hour'`); err != nil {
			t.Fatal(err)
		}
		if err := sender.Deliver(ctx); err != nil {
			t.Fatal(err)
		}
		if err := sender.Close(ctx); err != nil {
			t.Fatal(err)
		}
		return phone.take()
	}

	// A pending action: Approve, Reject and Open.
	run, err := v.s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: v.e.Current().Config.Agents["a"].SHA, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = v.s.ClaimRun(ctx, "w")
	ids, err := v.s.RecordOutcome(ctx, run, "a", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{{Type: "notify", Params: json.RawMessage(`{}`), Reason: "x", Rule: "approve"}}})
	if err != nil {
		t.Fatal(err)
	}
	got := deliver()
	if len(got) != 1 || got[0].Tag != "action:"+ids[0] || len(got[0].Actions) != 3 {
		t.Fatalf("pushed %+v, want the action with three buttons", got)
	}
	acts := map[string]push.Action{}
	for _, a := range got[0].Actions {
		acts[a.Action] = a
	}
	prefix := v.srv.URL + push.ActionPath + "?t="
	if acts["approve"].Title != "Approve" || !strings.HasPrefix(acts["approve"].URL, prefix) || acts["reject"].Title != "Reject" ||
		!strings.HasPrefix(acts["reject"].URL, prefix) || acts["open"].Title != "Open" || acts["open"].URL != "" {
		t.Fatalf("actions = %+v", got[0].Actions)
	}

	if code, _ := post(t, strings.Replace(acts["approve"].URL, "?t=", "?t=x", 1)); code != 403 {
		t.Fatalf("a tampered link = %d", code)
	}
	if code, body := post(t, acts["approve"].URL); code != 200 || body["ok"] != true || body["decision"] != "approve" {
		t.Fatalf("approve = %d %v", code, body)
	}
	a, _ := v.s.GetAction(ctx, ids[0])
	if a.Status != "approved" || a.DecidedBy == nil || *a.DecidedBy != "kanishk" {
		t.Fatalf("action = %+v", a)
	}
	// One notification decides once: its other button, and the same one again, are refused.
	for _, u := range []string{acts["reject"].URL, acts["approve"].URL} {
		if code, _ := post(t, u); code != 409 {
			t.Fatalf("a used link = %d", code)
		}
	}
	// The decision closed the row, so the device is told to drop the notification, once.
	if got := deliver(); len(got) != 1 || got[0].Kind != push.KindClose || got[0].Tag != "action:"+ids[0] || got[0].Title != "" || got[0].Actions != nil {
		t.Fatalf("after deciding, pushed %+v, want one close", got)
	}
	if got := deliver(); len(got) != 0 {
		t.Fatalf("a second close: %+v", got)
	}

	// A permission request: Approve (once), Deny and Open.
	var conv string
	_ = v.s.Pool.QueryRow(ctx, `SELECT conversation_id::text FROM runs WHERE id = $1`, run).Scan(&conv)
	ap, err := v.s.CreateApproval(ctx, store.NewApproval{RunID: run, Attempt: 1, ConversationID: conv, Agent: "a", RequestID: "q1",
		Kind: store.ApprovalKindTool, Tool: "Bash", Level: "ask", Input: json.RawMessage(`{}`), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	got = deliver()
	if len(got) != 1 || got[0].Tag != "approval:"+ap.ID || len(got[0].Actions) != 3 || got[0].Actions[1].Action != "deny" || got[0].Actions[1].Title != "Deny" {
		t.Fatalf("pushed %+v, want the permission request with Approve, Deny, Open", got)
	}
	if code, body := post(t, got[0].Actions[1].URL); code != 200 || body["decision"] != "deny" {
		t.Fatalf("deny = %d %v", code, body)
	}
	if d, _ := v.s.GetApproval(ctx, ap.ID); d.State != store.ApprovalDenied || d.DecidedBy == nil || *d.DecidedBy != "kanishk" || d.Scope == nil || *d.Scope != store.ScopeOnce {
		t.Fatalf("approval = %+v", d)
	}
	if got := deliver(); len(got) != 1 || got[0].Kind != push.KindClose || got[0].Tag != "approval:"+ap.ID {
		t.Fatalf("after denying, pushed %+v", got)
	}
}
