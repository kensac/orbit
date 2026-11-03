package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// git runs git in orbit's own clone. Nothing outside the command line and
// this environment configures it: no system or global config, no hooks, no
// prompt, and a token only in the environment of the commands that need
// one (github.Token.GitAuthEnv), never on disk.
type git struct {
	bin  string
	dir  string
	home string
}

func (g git) run(ctx context.Context, auth []string, args ...string) (string, error) {
	full := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.symlinks=true", "-c", "advice.detachedHead=false"}, args...)
	cmd := exec.CommandContext(ctx, g.bin, full...)
	cmd.Dir = g.dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + g.home,
		"LANG=C.UTF-8",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
	}, auth...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("git %s: %s", args[0], lastLine(msg))
	}
	return out.String(), nil
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// BranchPrefix is the only ref namespace a build pushes to.
const BranchPrefix = "refs/heads/orbit/"

// Branch is a ticket's branch: orbit/ and the first eight hex characters of
// its id. Code derives it from the id alone, so a retry and reconciliation
// find the same branch, and no model ever names one.
func Branch(ticketID string) (string, error) {
	hex := strings.ReplaceAll(strings.ToLower(ticketID), "-", "")
	if len(hex) < 8 || strings.Trim(hex[:8], "0123456789abcdef") != "" {
		return "", fmt.Errorf("ticket id %q is not a uuid", ticketID)
	}
	return "orbit/" + hex[:8], nil
}

// LaneBranch is a race lane's branch: the ticket's branch and -<lane>
// (orbit/3f2a9c1e-b), so every lane of a race has its own branch and PR;
// an empty lane is the ticket's own branch.
func LaneBranch(ticketID, lane string) (string, error) {
	b, err := Branch(ticketID)
	if err != nil || lane == "" {
		return b, err
	}
	if len(lane) != 1 || lane < "a" || lane > "c" {
		return "", fmt.Errorf("lane %q is not a, b or c", lane)
	}
	return b + "-" + lane, nil
}

// refspec is the one refspec a build pushes: a commit to its ticket's
// branch (Branch, LaneBranch), forced, because a retry rebuilds the branch
// from the default branch. It cannot name anything outside
// refs/heads/orbit/.
func refspec(branch, sha string) (string, error) {
	if !shaRE.MatchString(sha) {
		return "", fmt.Errorf("%q is not a commit sha", sha)
	}
	ref := "refs/heads/" + branch
	if !strings.HasPrefix(ref, BranchPrefix) || len(ref) == len(BranchPrefix) || strings.ContainsAny(ref[len(BranchPrefix):], "/:.~^*?[\\ ") {
		return "", fmt.Errorf("refusing to push %q", ref)
	}
	return "+" + sha + ":" + ref, nil
}

// remoteHeads reads the default branch and its head, and the ticket
// branch's head if it exists, from the remote.
func (g git) remoteHeads(ctx context.Context, auth []string, branch string) (defBranch, defSHA, branchSHA string, err error) {
	out, err := g.run(ctx, auth, "ls-remote", "--symref", "origin", "HEAD", "refs/heads/"+branch)
	if err != nil {
		return "", "", "", err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 3 && f[0] == "ref:" && f[2] == "HEAD":
			defBranch = strings.TrimPrefix(f[1], "refs/heads/")
		case len(f) == 2 && f[1] == "HEAD":
			defSHA = f[0]
		case len(f) == 2 && f[1] == "refs/heads/"+branch:
			branchSHA = f[0]
		}
	}
	if defBranch == "" || !shaRE.MatchString(defSHA) {
		return "", "", "", errors.New("the remote did not report its default branch")
	}
	return defBranch, defSHA, branchSHA, nil
}

// fetch brings one commit's tree (depth 1) into the clone under a local
// ref.
func (g git) fetch(ctx context.Context, auth []string, remoteRef, localRef string) error {
	return g.fetchDepth(ctx, auth, remoteRef, localRef, 1)
}

// fetchDepth is fetch with depth commits of history (2 reads a commit's
// parent too).
func (g git) fetchDepth(ctx context.Context, auth []string, remoteRef, localRef string, depth int) error {
	_, err := g.run(ctx, auth, "fetch", "--quiet", "--no-tags", fmt.Sprintf("--depth=%d", depth), "origin", "+"+remoteRef+":"+localRef)
	return err
}
