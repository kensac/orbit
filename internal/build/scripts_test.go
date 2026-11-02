package build

import (
	"errors"
	"strings"
	"testing"
)

func TestOnlyTrustedScriptsRun(t *testing.T) {
	const setup, cleanup = "echo prepared > setup.out\n", "rm setup.out\n"
	w := newWorld(t)
	onMain(t, w.bare, "", map[string]string{".orbit/build.yaml": "setup: |\n  echo prepared > setup.out\ncleanup: |\n  rm setup.out\n"})

	j := w.job("77777777-7777-4777-8777-777777777777", write("/work/hello.txt", "hi\n"))
	j.Policy.TrustedScripts = []string{ScriptHash(setup)}
	_, err := w.runner.Run(ctx, j, w.deps())
	if !errors.Is(err, ErrPolicy) || !strings.Contains(err.Error(), "cleanup script") || !strings.Contains(err.Error(), ScriptHash(cleanup)) {
		t.Fatalf("an untrusted cleanup = %v", err)
	}
	if created, _, _ := w.sb.Counts(); created != 0 {
		t.Fatal("a sandbox was made for a build whose scripts are not trusted")
	}

	j.Policy.TrustedScripts = []string{ScriptHash(setup), ScriptHash(cleanup)}
	res, err := w.runner.Run(ctx, j, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Diff.Paths(); len(got) != 1 || got[0] != "hello.txt" {
		t.Fatalf("diff = %v: cleanup should have removed what setup made", got)
	}
	at := w.sb.Attaches()
	if len(at) != 3 || at[0].Argv[0] != "/bin/sh" || at[0].Argv[2] != setup || len(at[0].Env) != 0 || at[2].Argv[2] != cleanup {
		t.Fatalf("attaches = %+v", at)
	}
	if !w.hasEvent(`orbit.build {"exit":0`) {
		t.Fatalf("no script event: %v", w.events)
	}
}

func TestAFailingSetupFailsTheBuild(t *testing.T) {
	w := newWorld(t)
	onMain(t, w.bare, "", map[string]string{".orbit/build.yaml": "setup: \"echo nope >&2; exit 3\"\n"})
	j := w.job("77777777-7777-4777-8777-777777777777", write("/work/hello.txt", "hi\n"))
	j.Policy.TrustedScripts = []string{ScriptHash("echo nope >&2; exit 3")}
	if _, err := w.runner.Run(ctx, j, w.deps()); err == nil || !strings.Contains(err.Error(), "setup script exited 3: nope") {
		t.Fatalf("err = %v", err)
	}
	if _, destroyed, open := w.sb.Counts(); destroyed != 1 || open != 0 {
		t.Fatal("the sandbox outlived a failed setup")
	}
}

func TestScriptsFileIsStrict(t *testing.T) {
	w := newWorld(t)
	onMain(t, w.bare, "", map[string]string{".orbit/build.yaml": "setup: x\npostinstall: y\n"})
	if _, err := w.runner.Run(ctx, w.job("77777777-7777-4777-8777-777777777777"), w.deps()); err == nil || !strings.Contains(err.Error(), "postinstall") {
		t.Fatalf("an unknown field = %v", err)
	}
}
