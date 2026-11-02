package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	token, err := Login(context.Background(), hc, srv.URL, "public-client", &notice)
	if err != nil || token != "private-access-token" {
		t.Fatalf("login %q %v", token, err)
	}
	if strings.Contains(notice.String(), "private-") || !strings.Contains(notice.String(), "ABC-123") {
		t.Fatalf("unsafe notice %q", notice.String())
	}
}
