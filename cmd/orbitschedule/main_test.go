package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPreviewCommand(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"--timezone", "America/New_York", "--anchor", "2026-11-02T00:00:00-05:00", "--count", "2", "every weekday at 9am"}, &out, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Timezone string      `json:"timezone"`
		Next     []time.Time `json:"next_fires"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Timezone != "America/New_York" || len(result.Next) != 2 || result.Next[0].Format(time.RFC3339) != "2026-11-02T09:00:00-05:00" {
		t.Fatal(out.String())
	}
}
func TestPreviewRequiresExplicitValidArguments(t *testing.T) {
	for _, args := range [][]string{{"daily at 9am"}, {"--timezone", "Local", "daily at 9am"}, {"--timezone", "not/a/zone", "daily at 9am"}, {"--timezone", "UTC", "--count", "21", "daily at 9am"}, {"--timezone", "UTC", "--anchor", "2026-11-02", "daily at 9am"}, {"--timezone", "UTC", "daily at 25"}} {
		var out bytes.Buffer
		if err := run(args, &out, time.Now()); err == nil {
			t.Fatalf("accepted %s", strings.Join(args, " "))
		}
		if out.Len() != 0 {
			t.Fatal("invalid command printed a trigger")
		}
	}
}
