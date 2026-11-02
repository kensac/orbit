package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/sandbox"
)

// Control's probe reads a not-ready answer as one, with the proxies sandboxd named, and a sandboxd that does not answer as an error.
func TestProbeSandboxd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != sandbox.PathReadyz {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"ready":false,"checks":{"docker":"ok","network":"ok","proxy:orbit-egress":"orbit-egress is unhealthy"}}`))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	rd, err := probeSandboxd(context.Background(), addr, "tok")
	if err != nil || rd.Ready || len(rd.Proxies()) != 1 || rd.Proxies()["orbit-egress"] {
		t.Fatalf("readiness = %+v %v", rd, err)
	}
	srv.Close()
	if _, err := probeSandboxd(context.Background(), addr, "tok"); err == nil {
		t.Fatal("a closed sandboxd answered")
	}
	if _, err := probeSandboxd(context.Background(), addr, ""); err == nil {
		t.Fatal("no token is an error")
	}
}

func TestHealthcheckURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	if err := healthcheckCmd([]string{"--url", srv.URL + "/healthz"}); err != nil {
		t.Fatal(err)
	}
	if err := healthcheckCmd([]string{"--url", srv.URL + "/other"}); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v", err)
	}
	if err := healthcheckCmd([]string{"--url", srv.URL, "--ready"}); err == nil {
		t.Fatal("--url with --ready was accepted")
	}
}
