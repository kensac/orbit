package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/push"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/webpush"
)

func TestAttentionInbox(t *testing.T) {
	v := newEnv(t, nil)
	me := v.person("kanishk")
	id, err := v.s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: v.e.Current().Config.Agents["a"].SHA, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = v.s.ClaimRun(ctx, "w")
	if err := v.s.FinishRun(ctx, id, store.RunResult{Attempt: 1, Status: "failed", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	code, body := call(t, "GET", v.srv.URL+"/api/v1/attention", me, "")
	items, _ := body["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("attention = %d %v", code, body)
	}
	it := items[0].(map[string]any)
	if it["kind"] != "failure" || it["detail"] != "boom" || it["link"] != "/orbit/c/"+it["conversation_id"].(string) {
		t.Fatalf("item = %v", it)
	}
	if code, _ := call(t, "POST", v.srv.URL+"/api/v1/attention/"+it["id"].(string)+"/dismiss", me, ""); code != 200 {
		t.Fatalf("dismiss = %d", code)
	}
	if code, _ := call(t, "POST", v.srv.URL+"/api/v1/attention/not-a-uuid/dismiss", me, ""); code != 400 {
		t.Fatalf("dismiss a bad id = %d", code)
	}
	if _, body := call(t, "GET", v.srv.URL+"/api/v1/attention", me, ""); len(body["items"].([]any)) != 0 {
		t.Fatalf("after dismiss = %v", body)
	}

	// Push is off without keys: the inbox works, subscribing is refused.
	if _, body := call(t, "GET", v.srv.URL+"/api/v1/push", me, ""); body["enabled"] != false {
		t.Fatalf("push = %v", body)
	}
	if code, _ := call(t, "POST", v.srv.URL+"/api/v1/push/subscriptions", me, `{}`); code != http.StatusServiceUnavailable {
		t.Fatalf("subscribe without keys = %d", code)
	}
}

func TestPushSubscriptionsAPI(t *testing.T) {
	pub, priv, _ := webpush.GenerateKeys()
	keys, _ := webpush.ParseKeys(pub, priv, "mailto:a@b.c")
	v := newEnv(t, func(a *api.Server, _ *http.Server) {
		a.Push = &push.Sender{Store: a.Store, Keys: keys, Client: http.DefaultClient, LinkBase: "/orbit",
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	})
	me := v.person("kanishk")
	if _, body := call(t, "GET", v.srv.URL+"/api/v1/push", me, ""); body["enabled"] != true || body["public_key"] != pub {
		t.Fatalf("push = %v", body)
	}
	sub := `{"endpoint":"https://fcm.googleapis.com/fcm/send/abc","keys":{"p256dh":"k","auth":"a"},"label":"Pixel"}`
	if code, body := call(t, "POST", v.srv.URL+"/api/v1/push/subscriptions", me, `{"endpoint":"http://10.0.0.1/x","keys":{"p256dh":"k","auth":"a"}}`); code != 400 {
		t.Fatalf("an endpoint off the push services = %d %v", code, body)
	}
	code, body := call(t, "POST", v.srv.URL+"/api/v1/push/subscriptions", me, sub)
	if code != 201 || body["id"] == "" {
		t.Fatalf("subscribe = %d %v", code, body)
	}
	_, body = call(t, "GET", v.srv.URL+"/api/v1/push/subscriptions", me, "")
	subs := body["subscriptions"].([]any)
	if len(subs) != 1 || subs[0].(map[string]any)["author"] != "kanishk" || subs[0].(map[string]any)["p256dh"] != nil {
		t.Fatalf("subscriptions = %v (keys must not be returned)", body)
	}
	if _, body := call(t, "GET", v.srv.URL+"/api/v1/push/subscriptions", v.person("someone"), ""); len(body["subscriptions"].([]any)) != 0 {
		t.Fatalf("another person's devices = %v", body)
	}
	if code, _ := call(t, "DELETE", v.srv.URL+"/api/v1/push/subscriptions", me, sub); code != 200 {
		t.Fatalf("unsubscribe = %d", code)
	}
	if code, _ := call(t, "DELETE", v.srv.URL+"/api/v1/push/subscriptions", me, sub); code != 404 {
		t.Fatalf("unsubscribe twice = %d", code)
	}
	if code, body := call(t, "POST", v.srv.URL+"/api/v1/push/test", me, ""); code != 200 || body["sent"] != float64(0) {
		t.Fatalf("test with no devices = %d %v", code, body)
	}
}
