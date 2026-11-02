package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/auth"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/store"
)

func status(t *testing.T, u string) int {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestReadinessFollowsTheDrain(t *testing.T) {
	d := engine.NewDrain()
	v := newEnv(t, func(a *api.Server, _ *http.Server) { a.Drain = d })
	for _, base := range []string{v.srv.URL, v.internal.URL} {
		if c := status(t, base+"/readyz"); c != 200 {
			t.Fatalf("%s/readyz = %d before draining", base, c)
		}
	}
	// Only the host's own listener can take the instance out of service.
	if code, _ := call(t, "POST", v.srv.URL+"/api/v1/drain", v.person("kanishk"), ""); code == 202 || d.Draining() {
		t.Fatalf("the public listener drained the instance: %d", code)
	}
	if code, _ := call(t, "POST", v.internal.URL+"/api/v1/drain", "", ""); code != 401 || d.Draining() {
		t.Fatalf("drain without a token = %d", code)
	}
	code, body := call(t, "POST", v.internal.URL+"/api/v1/drain", token, "")
	if code != 202 || body["started"] != true || !d.Draining() {
		t.Fatalf("drain = %d %v", code, body)
	}
	if _, body := call(t, "POST", v.internal.URL+"/api/v1/drain", token, ""); body["started"] != false {
		t.Fatalf("a second drain started again: %v", body)
	}
	for _, base := range []string{v.srv.URL, v.internal.URL} {
		if c := status(t, base+"/readyz"); c != 503 {
			t.Fatalf("%s/readyz = %d while draining, want 503", base, c)
		}
		// Alive, and still serving whoever is connected.
		if c := status(t, base+"/healthz"); c != 200 {
			t.Fatalf("%s/healthz = %d while draining", base, c)
		}
	}
	if code, _ := call(t, "GET", v.internal.URL+"/api/v1/agents", token, ""); code != 200 {
		t.Fatalf("the API stopped while draining: %d", code)
	}
}

// otherInstance is a second orbit's public listener on the same database.
func otherInstance(t *testing.T, v *env) *env {
	t.Helper()
	ver, err := auth.New(auth.Config{Issuers: []string{v.is.URL}, ClientIDs: []string{clientID}, Group: group})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	b := &api.Server{Store: v.s, Engine: v.e, Token: token, Verifier: ver, Wake: func() {}, Stop: stop,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	hs := httptest.NewUnstartedServer(nil)
	fastStreams(b, hs.Config)
	hs.Config.Handler = b.Handler()
	hs.Start()
	t.Cleanup(func() { close(stop); hs.Close() })
	w := *v
	w.srv = hs
	return &w
}

func TestAStreamContinuesOnTheOtherInstance(t *testing.T) {
	old := newEnv(t, fastStreams)
	run := enqueue(t, old)
	_, a1 := claim(t, old, "claude-code")
	appendEvents(t, old, run, a1, assistantText("Reading."),
		assistantTool("Read", `{"file_path":"/data/runs/`+run+`/inputs/transactions.json"}`))
	ch, cancel := openStream(t, old, run)
	expect(t, ch, "status", `{"attempt":1,"epoch":1,"status":"running"}`)
	expect(t, ch, "text", `{"text":"Reading."}`)
	last := expect(t, ch, "tool", `{"name":"Read","summary":"inputs/transactions.json"}`)
	// The old instance goes away; traefik sends the reconnect to the new
	// one, which knows nothing of the stream but its Last-Event-ID.
	cancel()
	appendEvents(t, old, run, a1, assistantText("Summing."))

	ch2, _ := resumeStream(t, otherInstance(t, old), run, last.ID)
	expect(t, ch2, "status", `{"attempt":1,"epoch":1,"status":"running"}`)
	expect(t, ch2, "text", `{"text":"\n\nSumming."}`)
	if _, err := old.s.RecordOutcome(ctx, run, "a", store.RunResult{Attempt: a1, Status: "succeeded", Report: "r"},
		store.Outcome{}); err != nil {
		t.Fatal(err)
	}
	expect(t, ch2, "status", `{"attempt":1,"epoch":1,"status":"succeeded"}`)
	expect(t, ch2, "done", "")
}

func TestARecoveredAttemptContinuesTheNarration(t *testing.T) {
	v := newEnv(t, fastStreams)
	run := enqueue(t, v)
	_, a1 := claim(t, v, "claude-code")
	appendEvents(t, v, run, a1, assistantText("Reading."))
	ch, _ := openStream(t, v, run)
	expect(t, ch, "status", `{"attempt":1,"epoch":1,"status":"running"}`)
	expect(t, ch, "text", `{"text":"Reading."}`)

	// Interrupted; the next attempt resumes its session.
	if err := v.s.HandBack(ctx, run, a1, nil, nil); err != nil {
		t.Fatal(err)
	}
	expect(t, ch, "status", `{"attempt":1,"epoch":1,"status":"queued"}`)
	_, a2 := claim(t, v, "claude-code")
	if err := v.s.SetRecovery(ctx, run, a2, a1, ""); err != nil {
		t.Fatal(err)
	}
	expect(t, ch, "status", `{"attempt":2,"continues":1,"epoch":1,"status":"running"}`)
	appendEvents(t, v, run, a2, assistantText("Summing."))
	// The prose goes on where it was: a chunk after the first, not a new start.
	expect(t, ch, "text", `{"text":"\n\nSumming."}`)

	// A client connecting now sees the whole narration, both attempts.
	late, _ := openStream(t, v, run)
	expect(t, late, "status", `{"attempt":2,"continues":1,"epoch":1,"status":"running"}`)
	if e := expect(t, late, "text", `{"text":"Reading."}`); e.ID != "1.2" {
		t.Fatalf("id = %s", e.ID)
	}
	if e := expect(t, late, "text", `{"text":"\n\nSumming."}`); e.ID != "1.5" {
		t.Fatalf("id = %s", e.ID)
	}
	// And one reconnecting after the first attempt's prose gets only the rest.
	again, _ := resumeStream(t, v, run, "1.2")
	expect(t, again, "status", `{"attempt":2,"continues":1,"epoch":1,"status":"running"}`)
	expect(t, again, "text", `{"text":"\n\nSumming."}`)
}
