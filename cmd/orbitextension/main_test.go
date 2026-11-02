package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestValidateCommand(t *testing.T) {
	var out bytes.Buffer
	if e := run([]string{"validate", "../../internal/extensions/testdata/reducer"}, &out); e != nil {
		t.Fatal(e)
	}
	var result struct {
		Digest string `json:"sha256"`
	}
	if e := json.Unmarshal(out.Bytes(), &result); e != nil || len(result.Digest) != 64 {
		t.Fatalf("invalid result %s: %v", out.String(), e)
	}
	for _, args := range [][]string{nil, {"install", "anything"}, {"validate", "/missing"}, {"validate", "x", "y"}} {
		out.Reset()
		if e := run(args, &out); e == nil {
			t.Fatalf("accepted %v", args)
		}
		if out.Len() != 0 {
			t.Fatal("wrote success output on failure")
		}
	}
}
