package runner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// controlCLI is a fake live CLI that answers control requests with success
// (logging each to requests) and every other line with a turn.
func controlCLI(t *testing.T) (bin, dir string) {
	t.Helper()
	bin, dir = fakeCLI(t, `printf '%s\n' "$@" >> "$(dirname "$0")/argvs"; echo --- >> "$(dirname "$0")/argvs"
echo start >> "$(dirname "$0")/starts"
n=0
while IFS= read -r line; do
  case "$line" in
  *'"control_request"'*)
    printf '%s\n' "$line" >> "$(dirname "$0")/requests"
    id=$(printf '%s' "$line" | sed 's/.*"request_id":"\([^"]*\)".*/\1/')
    echo '{"type":"control_response","response":{"subtype":"success","request_id":"'$id'","response":{}}}'
    ;;
  *)
    n=$((n+1))
    echo '{"type":"system","subtype":"init"}'
    echo '{"type":"result","subtype":"success","is_error":false,"structured_output":{"n":'$n'},"total_cost_usd":'$n',"session_id":"live-1"}'
    ;;
  esac
done`)
	stripStdinCapture(t, bin)
	return bin, dir
}

func TestALiveProcessTakesAConversationsModelAndEffort(t *testing.T) {
	bin, dir := controlCLI(t)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin, Model: "opus"}, orbitEnv)
	live := NewLive(MaxLive)
	defer live.Close()
	home, work := t.TempDir(), t.TempDir()
	run := func(resume, model, effort string) {
		t.Helper()
		var got started
		s := session(home, resume, 0, &got)
		s.Live, s.Open, s.LiveIdle = live, true, time.Minute
		if _, err := r.Run(context.Background(), Request{Workdir: work, Prompt: "p", OutputSchema: json.RawMessage(`{}`),
			Session: s, Model: model, Effort: effort}, func(Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	run("", "", "")
	run("live-1", "sonnet", "high")
	run("live-1", "", "")
	if n := len(lines(t, filepath.Join(dir, "starts"))); n != 1 {
		t.Fatalf("starts = %d, want one process for every model", n)
	}
	var subtypes []string
	for _, l := range lines(t, filepath.Join(dir, "requests")) {
		var m struct {
			Request map[string]any `json:"request"`
		}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(m.Request)
		subtypes = append(subtypes, string(b))
	}
	want := []string{
		`{"model":"sonnet","subtype":"set_model"}`,
		`{"settings":{"effortLevel":"high"},"subtype":"apply_flag_settings"}`,
		// Cleared: the runner's own model, the model's default effort.
		`{"model":"opus","subtype":"set_model"}`,
		`{"settings":{"effortLevel":null},"subtype":"apply_flag_settings"}`,
	}
	if strings.Join(subtypes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("control requests:\n%s\nwant:\n%s", strings.Join(subtypes, "\n"), strings.Join(want, "\n"))
	}

	// A process started for a conversation with a model is started with it,
	// after the runner's own (the last value wins).
	live.Close()
	live2 := NewLive(MaxLive)
	defer live2.Close()
	var got started
	s := session(home, "live-1", 0, &got)
	s.Live, s.Open, s.LiveIdle = live2, true, time.Minute
	if _, err := r.Run(context.Background(), Request{Workdir: work, Prompt: "p", OutputSchema: json.RawMessage(`{}`),
		Session: s, Model: "haiku", Effort: "low"}, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	argvs := strings.Split(read(t, filepath.Join(dir, "argvs")), "---\n")
	last := argvs[len(argvs)-2]
	if i, j := strings.Index(last, "--model\nopus\n"), strings.Index(last, "--model\nhaiku\n--effort\nlow\n"); i < 0 || j < i {
		t.Fatalf("argv:\n%s", last)
	}
	if strings.Contains(argvs[0], "--effort") {
		t.Fatalf("the first process was started with an effort:\n%s", argvs[0])
	}
}
