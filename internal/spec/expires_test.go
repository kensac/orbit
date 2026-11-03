package spec

import (
	"strings"
	"testing"
	"time"
)

// An expiry date is an unquoted YAML date or a string; a bad one or a name that is not a variable fails the load.
func TestExpires(t *testing.T) {
	cfg, err := Load(write(t, map[string]string{"orbit.yaml": base + "expires:\n  CLAUDE_CODE_OAUTH_TOKEN: 2027-10-31\n  OTHER: \"2027-01-02\"\n"}))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Deployment.Expiries()
	if !got["CLAUDE_CODE_OAUTH_TOKEN"].Equal(time.Date(2027, 10, 31, 0, 0, 0, 0, time.UTC)) || len(got) != 2 {
		t.Fatalf("expiries = %v", got)
	}
	_, err = Load(write(t, map[string]string{"orbit.yaml": base + "expires:\n  lower: 2027-10-31\n  TOKEN: next year\n"}))
	for _, want := range []string{`expires "lower": the name is an environment variable`, `expires TOKEN: "next year" is not a date (YYYY-MM-DD)`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
}
