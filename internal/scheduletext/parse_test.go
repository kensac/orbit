package scheduletext

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/kensac/orbit/internal/spec"
)

func location(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
func instant(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestPhrases(t *testing.T) {
	loc := location(t, "America/New_York")
	anchor := instant(t, "2026-11-02T00:00:00-05:00")
	for _, tc := range []struct{ text, rule, next string }{
		{"daily at 09:15", "FREQ=DAILY", "2026-11-02T09:15:00-05:00"},
		{"  Every   Weekday at 9am ", "BYDAY=MO,TU,WE,TH,FR", "2026-11-02T09:00:00-05:00"},
		{"every sunday at 12am", "BYDAY=SU", "2026-11-08T00:00:00-05:00"},
		{"every friday at 12:30pm", "BYDAY=FR", "2026-11-06T12:30:00-05:00"},
		{"every other monday at 8pm", "INTERVAL=2;BYDAY=MO", "2026-11-02T20:00:00-05:00"},
		{"the 21st of every month at 23:59", "BYMONTHDAY=21", "2026-11-21T23:59:00-05:00"},
		{"every month on the 31st at 09:00", "BYMONTHDAY=31", "2026-12-31T09:00:00-05:00"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			tr, err := Parse(tc.text, anchor, loc)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(tr.RRule, tc.rule) {
				t.Fatal(tr)
			}
			s, err := spec.TriggerSchedule(tr, loc)
			if err != nil {
				t.Fatal(err)
			}
			if got := s.Next(anchor).Format(time.RFC3339); got != tc.next {
				t.Fatalf("got %s want %s", got, tc.next)
			}
		})
	}
}
func TestDSTWallClockAndGap(t *testing.T) {
	loc := location(t, "America/New_York")
	for _, tc := range []struct{ text, anchor, first, second string }{
		{"every day at 09:00", "2026-03-07T00:00:00-05:00", "2026-03-07T09:00:00-05:00", "2026-03-08T09:00:00-04:00"},
		{"every day at 02:30", "2026-03-07T00:00:00-05:00", "2026-03-07T02:30:00-05:00", "2026-03-09T02:30:00-04:00"},
		{"every day at 09:00", "2026-10-31T00:00:00-04:00", "2026-10-31T09:00:00-04:00", "2026-11-01T09:00:00-05:00"},
	} {
		anchor := instant(t, tc.anchor)
		tr, err := Parse(tc.text, anchor, loc)
		if err != nil {
			t.Fatal(err)
		}
		s, err := spec.TriggerSchedule(tr, loc)
		if err != nil {
			t.Fatal(err)
		}
		a := s.Next(anchor)
		b := s.Next(a)
		if a.Format(time.RFC3339) != tc.first || b.Format(time.RFC3339) != tc.second {
			t.Fatalf("%s: %s %s", tc.text, a, b)
		}
	}
}
func TestTimezoneAndFortnightAnchor(t *testing.T) {
	anchor := instant(t, "2026-11-02T02:00:00Z")
	loc := location(t, "America/Los_Angeles")
	tr, err := Parse("every other sunday at 8pm", anchor, loc)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Start != "2026-11-01T00:00:00" {
		t.Fatalf("anchor uses wrong local date: %s", tr.Start)
	}
	s, err := spec.TriggerSchedule(tr, loc)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Next(anchor)
	b := s.Next(a)
	if a.Format(time.RFC3339) != "2026-11-01T20:00:00-08:00" || b.Format(time.RFC3339) != "2026-11-15T20:00:00-08:00" {
		t.Fatalf("%s %s", a, b)
	}
}
func TestRejectsAmbiguityAndUnsupportedText(t *testing.T) {
	for _, text := range []string{
		"every weekday at 8", "daily at 9:00", "daily at noon", "daily at 24:00", "daily at 00am", "daily at 13pm", "daily at 09:60", "daily at 9:60pm", "daily at 9am EST", "every mon at 9am", "every weekend at 9am", "every other week at 9am", "the 11st of every month at 9am", "the 0th of every month at 9am", "the 32nd of every month at 9am", "the 01st of every month at 9am", "daily at 9am then send email",
	} {
		if _, err := Parse(text, time.Now(), time.UTC); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
	if _, err := Parse("daily at 9am", time.Now(), time.Local); err == nil {
		t.Fatal("accepted host-dependent timezone")
	}
	if _, err := Parse("daily at 9am", time.Now(), nil); err == nil {
		t.Fatal("accepted missing timezone")
	}
	if _, err := Parse("daily at 9am", time.Time{}, time.UTC); err == nil {
		t.Fatal("accepted missing anchor")
	}
}
