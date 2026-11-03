package boxkeep_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/boxkeep"
	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

const sbToken = "sandbox-token-sandbox-token-sandbox-token"

func TestParsePollUsage(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 0, "0": 0, "off": 0, "10m": 10 * time.Minute} {
		if got, err := boxkeep.ParsePollUsage(v); err != nil || got != want {
			t.Errorf("%q: %v %v", v, got, err)
		}
	}
	for _, v := range []string{"1m", "soon", "-5m"} {
		if _, err := boxkeep.ParsePollUsage(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

// fakeBoxes answers a box's status and its accounts' polls.
type fakeBoxes struct {
	mu      sync.Mutex
	running map[string][]string
	ids     map[string]string
	polled  []string
	at      time.Time
	// tokens are the gh tokens written, "agent=token", and "agent=" for a
	// clear.
	tokens []string
}

func (f *fakeBoxes) server(t *testing.T) *sandbox.Client {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/boxes/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		st := sandbox.BoxStatus{Name: r.PathValue("name"), State: sandbox.BoxNone}
		if mounts, ok := f.running[r.PathValue("name")]; ok {
			st.State, st.Accounts, st.ID = sandbox.BoxRunning, mounts, f.ids[r.PathValue("name")]
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("POST /v1/boxes/{name}/accounts/{slot}/usage/poll", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.polled = append(f.polled, r.PathValue("name")+"/"+r.PathValue("slot"))
		pct := 9.0
		_ = json.NewEncoder(w).Encode(sandbox.AccountUsage{Source: sandbox.UsagePoll, ObservedAt: &f.at,
			Windows: []sandbox.UsageWindow{{Window: "five_hour", UsedPercentage: &pct}}})
	})
	mux.HandleFunc("PUT /v1/boxes/{name}/gh-token", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.tokens = append(f.tokens, r.PathValue("name")+"="+string(b))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/boxes/{name}/gh-token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.tokens = append(f.tokens, r.PathValue("name")+"=")
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+sbToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := sandbox.NewClient(strings.TrimPrefix(srv.URL, "http://"), sbToken)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestPollOnce polls only logged-in accounts the running boxes of box
// agents mount, and keeps the newest observation.
func TestPollOnce(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	box, _ := s.EnsureBox(ctx, "orbit")
	gone, _ := s.EnsureBox(ctx, "retired")
	stopped, _ := s.EnsureBox(ctx, "stopped")
	in, _ := s.CreateAccount(ctx, box.ID, "in", "k")
	out, _ := s.CreateAccount(ctx, box.ID, "logged out", "k")
	unmounted, _ := s.CreateAccount(ctx, box.ID, "unmounted", "k")
	other, _ := s.CreateAccount(ctx, gone.ID, "x", "k")
	idle, _ := s.CreateAccount(ctx, stopped.ID, "y", "k")
	for _, id := range []string{in.ID, unmounted.ID, other.ID, idle.ID} {
		if _, err := s.RecordAccountLogin(ctx, id, storeLogin()); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeBoxes{running: map[string][]string{"orbit": {in.Slot, out.Slot}, "retired": {other.Slot}},
		at: time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)}
	k := &boxkeep.Keeper{Store: s, Boxes: func() (*sandbox.Client, error) { return f.server(t), nil },
		Agents: func() []string { return []string{"orbit", "stopped"} }, PollUsage: 10 * time.Minute,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	k.PollOnce(ctx)
	if len(f.polled) != 1 || f.polled[0] != "orbit/"+in.Slot {
		t.Fatalf("polled %v", f.polled)
	}
	a, _ := s.GetAccount(ctx, in.ID)
	var u sandbox.AccountUsage
	if err := json.Unmarshal(a.Usage, &u); err != nil || u.Source != "poll" || !u.ObservedAt.Equal(f.at) || len(u.Windows) != 1 {
		t.Fatalf("usage %s %v", a.Usage, err)
	}
	if a, _ := s.GetAccount(ctx, out.ID); string(a.Usage) != "null" {
		t.Fatalf("a logged-out account's usage %s", a.Usage)
	}
	// An older observation never replaces a newer one.
	older := f.at.Add(-time.Hour)
	if ok, err := boxkeep.RecordUsage(ctx, s, in.ID, sandbox.AccountUsage{Source: "statusline", ObservedAt: &older}); err != nil || ok {
		t.Fatalf("older stored: %v %v", ok, err)
	}
	if ok, _ := boxkeep.RecordUsage(ctx, s, in.ID, sandbox.AccountUsage{Source: "statusline"}); ok {
		t.Fatal("no observation stored")
	}
}

func storeLogin() store.AccountLogin {
	return store.AccountLogin{LoggedIn: true, Email: "k@example.com"}
}

// TestGHOnce keeps the box token in running box agents' boxes: written
// once, again when the box is recreated or GHRewrite has passed, minted at
// most every ten minutes, and removed once when switched off.
func TestGHOnce(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	_, _ = s.EnsureBox(ctx, "orbit")
	_, _ = s.EnsureBox(ctx, "idle")
	_, _ = s.EnsureBox(ctx, "retired")
	f := &fakeBoxes{running: map[string][]string{"orbit": nil, "retired": nil}, ids: map[string]string{"orbit": "c1", "retired": "c9"}}
	client := f.server(t)
	gh := githubtest.New(t)
	gh.InstallationRepos = []string{"kensac/orbit", "kensac/hetzner-vps-config"}
	now := time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)
	on := true
	cfg := &spec.GitHub{AppID: githubtest.AppID, Repositories: []string{"kensac/orbit"}, Box: &spec.GitHubBox{Token: &on}}
	k := &boxkeep.Keeper{Store: s, Boxes: func() (*sandbox.Client, error) { return client, nil },
		Agents: func() []string { return []string{"orbit", "idle"} },
		GitHub: func() *spec.GitHub { return cfg },
		App: func(*spec.GitHub) (*github.App, error) {
			return &github.App{ID: githubtest.AppID, Key: githubtest.Key, BaseURL: gh.URL(), HTTP: gh.Srv.Client(), Now: time.Now}, nil
		},
		Now: func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	written := func() []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]string(nil), f.tokens...)
	}
	mints := func() int {
		gh.Mu.Lock()
		defer gh.Mu.Unlock()
		n := 0
		for _, r := range gh.TokenReqs {
			if r["repositories"] != nil {
				n++
			}
		}
		return n
	}
	k.Once(ctx)
	if got := written(); len(got) != 1 || got[0] != "orbit="+githubtest.Token || mints() != 1 {
		t.Fatalf("first round: %v, %d mints", got, mints())
	}
	gh.Mu.Lock()
	if repos := gh.TokenReq["repositories"].([]any); len(repos) != 1 || repos[0] != "orbit" {
		t.Fatalf("the box token reaches %v", repos)
	}
	gh.Mu.Unlock()
	now = now.Add(time.Minute)
	k.Once(ctx)
	if len(written()) != 1 {
		t.Fatal("written again within GHRewrite")
	}
	// A recreated box gets the token at once, without a new mint.
	f.mu.Lock()
	f.ids["orbit"] = "c2"
	f.mu.Unlock()
	k.Once(ctx)
	if len(written()) != 2 || mints() != 1 {
		t.Fatalf("recreated box: %v, %d mints", written(), mints())
	}
	now = now.Add(boxkeep.GHRewrite)
	k.Once(ctx)
	if len(written()) != 3 || mints() != 2 {
		t.Fatalf("after GHRewrite: %v, %d mints", written(), mints())
	}
	// Switched off: removed once from each running box.
	off := false
	cfg.Box.Token = &off
	k.Once(ctx)
	k.Once(ctx)
	if got := written(); len(got) != 4 || got[3] != "orbit=" {
		t.Fatalf("switched off: %v", got)
	}
}
