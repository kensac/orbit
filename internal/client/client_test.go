package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestEndpointValidation(t *testing.T) {
	for _, s := range []string{"http://orbit.example", "https://user:secret@orbit.example", "https://orbit.example?token=x", "https://orbit.example#secret"} {
		if ValidateURL(s) == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	if ValidateURL("https://orbit.example") != nil {
		t.Fatal("valid endpoint rejected")
	}
}
func TestRequestRefusesRedirect(t *testing.T) {
	var leaked bool
	dest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer dest.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer person" {
			t.Error("missing token")
		}
		http.Redirect(w, r, dest.URL, http.StatusFound)
	}))
	defer srv.Close()
	hc := HTTPClient()
	hc.Transport = srv.Client().Transport
	c := Client{BaseURL: srv.URL, Token: "person", HTTP: hc}
	if c.Request(context.Background(), "GET", "agents", nil, io.Discard) == nil {
		t.Fatal("redirect accepted")
	}
	if leaked {
		t.Fatal("followed redirect")
	}
}
func TestMessage(t *testing.T) {
	b, _ := io.ReadAll(Message("hello", "request-id"))
	if strings.Contains(string(b), "author") || !strings.Contains(string(b), `"client_message_id":"request-id"`) {
		t.Fatalf("message %s", b)
	}
}
func TestSegment(t *testing.T) {
	for _, s := range []string{"", "..", "../agents", "x?y", "x%2fy", "x\\y"} {
		if _, e := Segment(s); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
func TestLoginRejectsForeignEndpoints(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"issuer":%q,"device_authorization_endpoint":"https://foreign.example/device","token_endpoint":"https://foreign.example/token"}`, srv.URL)
	}))
	defer srv.Close()
	hc := HTTPClient()
	hc.Transport = srv.Client().Transport
	if _, err := Login(context.Background(), hc, srv.URL, "device", io.Discard); err == nil {
		t.Fatal("accepted foreign device endpoints")
	}
}

func TestDeviceLogin(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			fmt.Fprintf(w, `{"issuer":%q,"device_authorization_endpoint":%q,"token_endpoint":%q}`, srv.URL, srv.URL+"/device", srv.URL+"/token")
		case "/device":
			if r.Method != "POST" {
				t.Error("device method")
			}
			_ = r.ParseForm()
			if r.Form.Get("client_id") != "public-client" || r.Form.Has("client_secret") {
				t.Error("device client")
			}
			fmt.Fprintf(w, `{"device_code":"private-device-code","user_code":"ABC-123","verification_uri":%q,"expires_in":30,"interval":1}`, srv.URL+"/verify")
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("device_code") != "private-device-code" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Error("token grant")
			}
			fmt.Fprint(w, `{"access_token":"private-access-token","token_type":"Bearer"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	hc := HTTPClient()
	hc.Transport = srv.Client().Transport
	var notice strings.Builder
	token, err := login(context.Background(), hc, srv.URL, "public-client", &notice, func(context.Context, time.Duration) error { return nil })
	if err != nil || token != "private-access-token" {
		t.Fatalf("login %q %v", token, err)
	}
	if strings.Contains(notice.String(), "private-") || !strings.Contains(notice.String(), "ABC-123") {
		t.Fatalf("unsafe notice %q", notice.String())
	}
}

func TestOAuthHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		body     string
		poll, ok bool
	}{
		{`{"access_token":"secret","token_type":"Bearer"}`, true, false},
		{`{"device_code":"secret"}`, false, false},
		{`{"error":"authorization_pending"}`, true, true},
		{`{"error":"authorization_pending","access_token":"secret"}`, true, false},
		{`{"error":"authorization_pending"}`, false, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			hc := HTTPClient()
			hc.Transport = srv.Client().Transport
			var out map[string]any
			err := form(context.Background(), hc, srv.URL, url.Values{}, &out, tc.poll)
			if (err == nil) != tc.ok {
				t.Fatalf("error %v, want success %v", err, tc.ok)
			}
		})
	}
}

func TestDevicePolling(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			var srv *httptest.Server
			polls := 0
			srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					fmt.Fprintf(w, `{"issuer":%q,"device_authorization_endpoint":%q,"token_endpoint":%q}`, srv.URL, srv.URL+"/device", srv.URL+"/token")
				case "/device":
					fmt.Fprintf(w, `{"device_code":"private","user_code":"ABC","verification_uri":%q,"expires_in":60}`, srv.URL+"/verify")
				case "/token":
					polls++
					w.WriteHeader(http.StatusBadRequest)
					e := []string{"authorization_pending", "slow_down", "access_denied"}[polls-1]
					fmt.Fprintf(w, `{"error":%q}`, e)
				}
			}))
			defer srv.Close()
			hc := HTTPClient()
			hc.Transport = srv.Client().Transport
			var waits []time.Duration
			token, err := login(context.Background(), hc, srv.URL, "public", io.Discard, func(ctx context.Context, d time.Duration) error {
				waits = append(waits, d)
				if cancel && len(waits) == 3 {
					return context.Canceled
				}
				return nil
			})
			if token != "" || err == nil {
				t.Fatalf("token %q error %v", token, err)
			}
			if len(waits) != 3 || waits[0] != 5*time.Second || waits[1] != 5*time.Second || waits[2] != 10*time.Second {
				t.Fatalf("waits %v", waits)
			}
			if cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("error %v", err)
			}
		})
	}
}
