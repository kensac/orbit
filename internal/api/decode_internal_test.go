package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecode: every body is decoded strictly, as one JSON value within its limit, and an empty body is the zero value only where the handler says the body is optional, whether or not the client sent a length.
func TestDecode(t *testing.T) {
	type into struct {
		By              string `json:"by"`
		ClientRequestID string `json:"client_request_id"`
	}
	for _, c := range []struct {
		name    string
		body    string
		chunked bool
		rule    bodyRule
		ok      bool
		want    into
		err     string
	}{
		{name: "known fields", body: `{"by":"k","client_request_id":"r"}`, rule: bodyRequired, ok: true, want: into{"k", "r"}},
		{name: "unknown field", body: `{"by":"k","client_requst_id":"r"}`, rule: bodyOptional, err: `unknown field \"client_requst_id\"`},
		{name: "malformed", body: `{"by":`, rule: bodyOptional, err: "unexpected EOF"},
		{name: "two values", body: `{"by":"k"} {"by":"j"}`, rule: bodyRequired, err: "one JSON value"},
		{name: "trailing garbage", body: `{"by":"k"}x`, rule: bodyRequired, err: "one JSON value"},
		{name: "trailing whitespace", body: "{\"by\":\"k\"}\n", rule: bodyRequired, ok: true, want: into{By: "k"}},
		{name: "not an object", body: `[]`, rule: bodyRequired, err: "cannot unmarshal"},
		{name: "too large", body: `{"by":"` + strings.Repeat("x", 100) + `"}`, rule: bodyRequired, err: "too large"},
		{name: "empty, optional", body: "", rule: bodyOptional, ok: true},
		{name: "empty, optional, chunked", body: "", chunked: true, rule: bodyOptional, ok: true},
		{name: "empty, required", body: "", rule: bodyRequired, err: "a JSON body is required"},
		{name: "empty, required, chunked", body: "", chunked: true, rule: bodyRequired, err: "a JSON body is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(c.body))
			if c.chunked {
				r.ContentLength = -1
			}
			w := httptest.NewRecorder()
			var got into
			ok := decode(w, r, 64, &got, c.rule)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (%d %s)", ok, c.ok, w.Code, w.Body)
			}
			if ok && got != c.want {
				t.Fatalf("decoded %+v, want %+v", got, c.want)
			}
			if !ok && (w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.err)) {
				t.Fatalf("got %d %s, want 400 naming %q", w.Code, w.Body, c.err)
			}
		})
	}
}

func TestQueryInt(t *testing.T) {
	for _, c := range []struct {
		query string
		ok    bool
		want  int
	}{{"", true, 0}, {"n=7", true, 7}, {"n=-2", true, -2}, {"n=abc", false, 0}, {"n=1.5", false, 0}, {"n=", true, 0}, {"n=99999999999999999999", false, 0}} {
		r := httptest.NewRequest(http.MethodGet, "/?"+c.query, nil)
		w := httptest.NewRecorder()
		n := 0
		ok := queryInt(w, r, "n", &n)
		if ok != c.ok || n != c.want {
			t.Errorf("%q: ok %v n %d, want %v %d", c.query, ok, n, c.ok, c.want)
		}
		if !ok && (w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "n: want an integer")) {
			t.Errorf("%q: %d %s", c.query, w.Code, w.Body)
		}
	}
}
