package main

import (
	"context"
	"io"
	"testing"
)

func TestRejectsBeforeAuthentication(t *testing.T) {
	for _, args := range [][]string{
		{}, {"capture", "run", "0", "unused"},
		{"-url", "http://example.test", "capture", "run", "1", "unused"},
		{"-url", "https://example.test", "-timeout", "11m", "capture", "run", "1", "unused"},
		{"-url", "https://example.test", "capture", "../run", "1", "unused"},
	} {
		if err := run(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
