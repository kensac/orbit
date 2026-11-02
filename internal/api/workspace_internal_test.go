package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func withPerson(r *http.Request, person string, groups ...string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{Person: person, Groups: groups}))
}

func TestWorkspaceRateIsPerPersonAndWorkspace(t *testing.T) {
	s := &Server{}
	r := withPerson(httptest.NewRequest("GET", "/", nil), "kanishk")
	for i := 0; i < writeRate; i++ {
		if !s.allow(httptest.NewRecorder(), r, "w1", "write", writeRate) {
			t.Fatalf("request %d refused within the burst", i)
		}
	}
	rec := httptest.NewRecorder()
	if s.allow(rec, r, "w1", "write", writeRate) {
		t.Fatal("over the rate allowed")
	}
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("refusal = %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if !s.allow(httptest.NewRecorder(), r, "w2", "write", writeRate) {
		t.Fatal("another workspace has its own bucket")
	}
	if !s.allow(httptest.NewRecorder(), r, "w1", "read", readRate) {
		t.Fatal("reads have their own bucket")
	}
	other := withPerson(httptest.NewRequest("GET", "/", nil), "someone")
	if !s.allow(httptest.NewRecorder(), other, "w1", "write", writeRate) {
		t.Fatal("another person has their own bucket")
	}
}

func TestWorkspaceGroup(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/", nil)
	if !s.inWorkspaceGroup(withPerson(req, "kanishk", "infra-admins")) {
		t.Fatal("unset, every verified person may write")
	}
	s.WorkspaceGroup = "workspace"
	if s.inWorkspaceGroup(withPerson(req, "kanishk", "infra-admins")) {
		t.Fatal("outside the workspace group")
	}
	if !s.inWorkspaceGroup(withPerson(req, "kanishk", "infra-admins", "workspace")) {
		t.Fatal("in the workspace group")
	}
	if !s.inWorkspaceGroup(withPerson(req, "")) {
		t.Fatal("the operator token")
	}
}
