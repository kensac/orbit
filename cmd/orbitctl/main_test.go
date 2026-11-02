package main

import (
	"encoding/json"
	"testing"
)

func TestCommands(t *testing.T) {
	for _, a := range [][]string{{"agents"}, {"conversations", "assistant"}, {"thread", "conversation"}, {"stream", "run"}, {"approve", "action"}, {"reject", "action"}, {"send", "conversation", "hello"}, {"send-agent", "assistant", "hello"}} {
		method, path, body, err := command(a)
		if err != nil || method == "" || path == "" {
			t.Fatalf("%v: %s %s %v", a, method, path, err)
		}
		if a[0] == "send" || a[0] == "send-agent" {
			var b map[string]any
			if json.Unmarshal([]byte(body), &b) != nil || b["body"] != "hello" || b["client_message_id"] == "" {
				t.Fatalf("message %s", body)
			}
		}
	}
}
func TestInvalidCommands(t *testing.T) {
	for _, a := range [][]string{{"unknown", "x"}, {"send", "x"}, {"approve", "../actions"}, {"agents", "extra"}, {"stream", "x", "extra"}} {
		if _, _, _, err := command(a); err == nil {
			t.Fatalf("accepted %v", a)
		}
	}
}
