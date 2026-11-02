package main

import (
	"context"
	"io"
	"testing"
)

func TestInvalidCommand(t *testing.T) {
	for _, args := range [][]string{{}, {"restore", "x"}, {"export", "assistant"}, {"verify", "missing.zip"}, {"-url", "http://unsafe", "export", "assistant", "new.zip"}, {"-url", "https://orbit.example", "-timeout", "0s", "export", "assistant", "new.zip"}} {
		if err := run(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
