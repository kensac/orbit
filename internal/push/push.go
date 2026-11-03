// Package push wakes a person's devices when something needs them: each
// attention row (store/attention.go) with push set becomes one Web Push to
// every registered device, sent straight to the browser's push service,
// except the devices of a person whose app is showing that conversation
// (Presence).
//
// A push says only which agent wants what and where to look: its title is
// the attention row's title (an agent name and a fixed phrase) and its link
// a path into the app. Reports, questions, errors and action parameters
// never leave in a push; the app shows them after sign-in.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/metrics"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/webpush"
)

const (
	// Gap is the least time between two pushes. What arrives inside it
	// waits and goes out as one summary, so a burst (a run asking three
	// questions, a schedule failing every agent) is one buzz.
	Gap = 20 * time.Second
	// MaxAge drops what waited longer than this to be pushed (control was
	// down): the inbox still lists it, and a day-old buzz misleads.
	MaxAge = 24 * time.Hour
	// TTL is how long a push service keeps a push for an offline device;
	// MaxAge for the same reason.
	TTL = MaxAge
	// Presence is how long a heartbeat (POST /presence, every 30 s) keeps
	// a conversation's pushes off its person's devices: they are looking
	// at it. Two heartbeats missed and it no longer does.
	Presence = 60 * time.Second
)

// Payload is the JSON the service worker receives. It is everything a
// push carries.
type Payload struct {
	Title string `json:"title"`
	// URL is a path in the app, resolved against the worker's origin.
	URL string `json:"url"`
	// Tag collapses notifications on the device: a later push with the same
	// tag replaces the earlier one.
	Tag   string `json:"tag"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// Sender sends pushes. Control instances only: it holds the VAPID key.
type Sender struct {
	Store  *store.Store
	Keys   *webpush.Keys
	Client *http.Client
	// LinkBase is the app's path for orbit ("/orbit"); links are built
	// under it (Link).
	LinkBase string
	Log      *slog.Logger
}

// FromEnv builds a Sender from ORBIT_VAPID_PUBLIC_KEY,
// ORBIT_VAPID_PRIVATE_KEY and ORBIT_VAPID_SUBJECT, and ORBIT_PUSH_LINK_BASE
// (default /orbit). Without the keys it returns nil: push is off and the
// inbox still works. Keys that are set but wrong are an error, so a typo
// fails the start instead of silently sending nothing.
func FromEnv(s *store.Store, log *slog.Logger) (*Sender, error) {
	pub, priv := os.Getenv("ORBIT_VAPID_PUBLIC_KEY"), os.Getenv("ORBIT_VAPID_PRIVATE_KEY")
	if pub == "" && priv == "" {
		return nil, nil
	}
	keys, err := webpush.ParseKeys(pub, priv, os.Getenv("ORBIT_VAPID_SUBJECT"))
	if err != nil {
		return nil, fmt.Errorf("ORBIT_VAPID_*: %w", err)
	}
	base := strings.TrimRight(os.Getenv("ORBIT_PUSH_LINK_BASE"), "/")
	if base == "" {
		base = "/orbit"
	}
	if !strings.HasPrefix(base, "/") {
		return nil, errors.New("ORBIT_PUSH_LINK_BASE: want a path starting with /")
	}
	return &Sender{Store: s, Keys: keys, Client: &http.Client{Timeout: 30 * time.Second}, LinkBase: base, Log: log}, nil
}

// Link is where a person acts on a row: its conversation, at the action or
// question or permission request when there is one (a fragment the app
// scrolls to).
func Link(base string, a store.Attention) string {
	l := base + "/c/" + a.ConversationID
	switch {
	case a.ActionID != nil: // an approval, or a delegated task's decision
		l += "#action-" + *a.ActionID
	case a.ApprovalID != nil: // a CLI permission request
		l += "#approval-" + *a.ApprovalID
	case a.Kind == store.AttentionQuestion && a.MessageID != nil:
		l += "#message-" + *a.MessageID
	}
	return l
}

// Deliver sends whatever is waiting, if the gap allows, to each person's
// devices: one row as itself, several as one summary that opens the inbox.
// A row of the conversation a person's app is showing (Presence) is left
// out of what that person is sent; their inbox and the thread show it.
func (s *Sender) Deliver(ctx context.Context) error {
	items, err := s.Store.ClaimPush(ctx, Gap, MaxAge)
	if err != nil || len(items) == 0 {
		return err
	}
	subs, err := s.Store.ListPushSubscriptions(ctx, "")
	if err != nil {
		return err
	}
	watching, err := s.Store.Watching(ctx, Presence)
	if err != nil {
		// Unknown presence never silences a push.
		s.Log.Warn("read presence", "err", err)
		watching = nil
	}
	for author, devices := range byAuthor(subs) {
		if send := unseen(items, watching[author]); len(send) > 0 {
			s.sendTo(ctx, devices, summarize(s.LinkBase, send))
		}
	}
	return nil
}

// unseen is items without those of the conversation a person is looking
// at ("" when they are not known to be looking at one).
func unseen(items []store.Attention, watching string) []store.Attention {
	if watching == "" {
		return items
	}
	var out []store.Attention
	for _, a := range items {
		if a.ConversationID != watching {
			out = append(out, a)
		}
	}
	return out
}

func byAuthor(subs []store.PushSubscription) map[string][]store.PushSubscription {
	out := map[string][]store.PushSubscription{}
	for _, sub := range subs {
		out[sub.Author] = append(out[sub.Author], sub)
	}
	return out
}

// Test sends one push to author's devices, so a person can see that
// subscribing worked. It returns how many devices accepted it.
func (s *Sender) Test(ctx context.Context, author string) (int, error) {
	return s.send(ctx, author, Payload{Title: "orbit notifications are on", URL: s.LinkBase + "/inbox", Tag: "orbit-test", Kind: "test"})
}

func summarize(base string, items []store.Attention) Payload {
	if len(items) == 1 {
		a := items[0]
		return Payload{Title: a.Title, URL: Link(base, a), Tag: a.Key, Kind: a.Kind, Count: 1}
	}
	var agents []string
	for _, a := range items {
		if !slices.Contains(agents, a.Agent) {
			agents = append(agents, a.Agent)
		}
	}
	return Payload{
		Title: fmt.Sprintf("%d things need you: %s", len(items), strings.Join(agents, ", ")),
		URL:   base + "/inbox", Tag: "orbit-inbox", Kind: "summary", Count: len(items),
	}
}

func (s *Sender) send(ctx context.Context, author string, p Payload) (int, error) {
	subs, err := s.Store.ListPushSubscriptions(ctx, author)
	if err != nil {
		return 0, err
	}
	return s.sendTo(ctx, subs, p), nil
}

// sendTo sends p to each device and records each result; it returns how
// many accepted it.
func (s *Sender) sendTo(ctx context.Context, subs []store.PushSubscription, p Payload) int {
	body, _ := json.Marshal(p)
	urgency := "high"
	if p.Kind == store.AttentionFailure || p.Kind == "test" {
		urgency = "normal"
	}
	sent := 0
	for _, sub := range subs {
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := s.Keys.Send(sendCtx, s.Client, webpush.Subscription{Endpoint: sub.Endpoint, P256DH: sub.P256DH, Auth: sub.Auth},
			webpush.Message{Payload: body, TTL: TTL, Topic: webpush.Topic(p.Tag), Urgency: urgency})
		cancel()
		metrics.Delivery("webpush", err)
		gone := errors.Is(err, webpush.ErrGone)
		if err != nil {
			s.Log.Warn("push failed", "subscription", sub.ID, "gone", gone, "err", err)
		} else {
			sent++
		}
		if rerr := s.Store.PushResult(context.WithoutCancel(ctx), sub.ID, err, gone); rerr != nil {
			s.Log.Error("record push", "err", rerr)
		}
	}
	return sent
}

// pushHosts are the push services orbit will post to. A subscription's
// endpoint is a URL the browser chose, and orbit sends to it with its own
// network position, so anything else is refused at registration.
var pushHosts = []string{
	"fcm.googleapis.com",        // Chrome, Edge on Android, Brave
	"android.googleapis.com",    // older Chrome
	"push.apple.com",            // Safari, iOS home-screen apps (web.push.apple.com)
	"notify.windows.com",        // Edge on Windows (*.notify.windows.com)
	"push.services.mozilla.com", // Firefox (updates.push.services.mozilla.com)
}

// AllowedEndpoint reports whether a subscription endpoint is an https URL
// on a known push service.
func AllowedEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("endpoint must be an https URL")
	}
	host := u.Hostname()
	for _, h := range pushHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return nil
		}
	}
	return fmt.Errorf("endpoint host %s is not a known push service", host)
}
