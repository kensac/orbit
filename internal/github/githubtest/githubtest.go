// Package githubtest is a fake of the slice of GitHub's REST API orbit uses,
// for tests. It verifies the App JWT and the installation token the way
// GitHub does, so a test passing against it means orbit authenticated
// correctly, and it records every write so a test can assert on them.
package githubtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Key is the App's private key; AppID its id.
var Key = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

// AppID is the fake App's id, the JWT's iss.
const AppID = 42

// Head is the PR's head commit.
const Head = "1111111111111111111111111111111111111111"

// Token is the installation token the fake mints.
const Token = "ghs_test"

// KeyPEM is Key as GitHub issues it.
func KeyPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(Key)}))
}

// Server is a fake GitHub whose state a test can change. Lock Mu to read
// or change fields while it serves.
type Server struct {
	Srv *httptest.Server

	Mu         sync.Mutex
	Granted    map[string]string // what access_tokens answers with; nil echoes the request
	TokenReq   map[string]any
	Revoked    []string
	PR         map[string]any
	Mergeable  []any // successive values of "mergeable"; the last repeats
	PRReads    int
	Protection map[string]any
	Rules      []any
	CheckRuns  []map[string]any
	Statuses   []map[string]any
	Patched    []map[string]string
	Merged     map[string]string
	Calls      []string

	// The build delivery's side. PR nil means no PR exists yet: creating
	// one makes it PR 7, with its head at BranchHead(ref).
	BranchHead func(ref string) string
	Created    []map[string]any
	// Compare answers compare/{base}...{head}, keyed "base...head";
	// "identical" when base and head are equal, "diverged" otherwise.
	Compare map[string]string
	// CompareFiles are the files every compare reports changed.
	CompareFiles []string
	// JobLogs are Actions job logs by job id, served through a redirect
	// as GitHub does.
	JobLogs map[int64]string
	// MergeSHA is the commit the merge creates.
	MergeSHA string
	// Reruns are the Actions jobs rerun, in order. A rerun puts the check
	// run with that id back in the queue, as GitHub's new attempt does,
	// unless RerunStatus is set: then it is refused with that status.
	Reruns      []int64
	RerunStatus int
	// CommitChecks are the check runs of commits other than the checked
	// one (a build's base), by sha.
	CommitChecks map[string][]map[string]any
	// Others are pull requests besides PR (7), by number: read and
	// patched, nothing else.
	Others map[int]map[string]any
	// Comments are the issue and pull request comments posted, each with
	// "number" set to the issue's.
	Comments []map[string]any
}

// New starts a fake, stopped when the test ends.
func New(t *testing.T) *Server {
	f := &Server{}
	f.Reset()
	f.Srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Srv.Close)
	return f
}

// Reset puts back the default state: an open, mergeable orbit PR whose
// required check passed and whose unrequired gitleaks check failed.
func (f *Server) Reset() {
	f.PR = map[string]any{
		"number": 7, "state": "open", "draft": false, "merged": false,
		"mergeable_state": "clean", "title": "Add a thing",
		"body": "What it does.\n\nGenerated with [Claude Code](https://x)\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n",
		"head": map[string]any{"ref": "orbit/abc123", "sha": Head, "repo": map[string]any{"full_name": "kensac/orbit"}},
		"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "kensac/orbit", "default_branch": "main"}},
	}
	f.Mergeable = []any{true}
	f.MergeSHA = "2222"
	f.Protection = map[string]any{"required_status_checks": map[string]any{"contexts": []string{"check"}}}
	f.CheckRuns = []map[string]any{
		{"name": "check", "status": "completed", "conclusion": "success", "app": map[string]any{"id": 15368}},
		{"name": "gitleaks", "status": "completed", "conclusion": "failure", "app": map[string]any{"id": 15368}},
	}
}

// URL is the fake API's base URL.
func (f *Server) URL() string { return f.Srv.URL }

func (f *Server) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Calls = append(f.Calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" && !strings.HasPrefix(r.URL.Path, "/joblogs/") {
		f.write(w, 400, map[string]string{"message": "no api version"})
		return
	}
	auth := r.Header.Get("Authorization")
	if id, ok := strings.CutPrefix(r.URL.Path, "/joblogs/"); ok {
		// The log's storage: a presigned URL, no GitHub credential.
		if auth != "" {
			f.write(w, 400, map[string]string{"message": "a credential was sent to the log storage"})
			return
		}
		var n int64
		_, _ = fmt.Sscan(id, &n)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(f.JobLogs[n]))
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/installation":
		if !f.validJWT(strings.TrimPrefix(auth, "Bearer ")) {
			f.write(w, 401, map[string]string{"message": "A JSON web token could not be decoded"})
			return
		}
		f.write(w, 200, map[string]any{"id": 99})
		return
	case r.Method == "POST" && r.URL.Path == "/app/installations/99/access_tokens":
		if !f.validJWT(strings.TrimPrefix(auth, "Bearer ")) {
			f.write(w, 401, map[string]string{"message": "bad jwt"})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&f.TokenReq)
		perms := map[string]any{"metadata": "read"}
		if f.Granted != nil {
			for k, v := range f.Granted {
				perms[k] = v
			}
		} else {
			for k, v := range f.TokenReq["permissions"].(map[string]any) {
				perms[k] = v
			}
		}
		f.write(w, 201, map[string]any{"token": Token, "expires_at": time.Now().Add(time.Hour), "permissions": perms})
		return
	}
	if auth != "token "+Token {
		f.write(w, 401, map[string]string{"message": "Bad credentials"})
		return
	}
	switch {
	case r.Method == "DELETE" && r.URL.Path == "/installation/token":
		f.Revoked = append(f.Revoked, auth)
		w.WriteHeader(204)
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/pulls" && r.URL.Query().Get("head") == "":
		// A plain list: PR and Others in the state asked for, paged.
		f.write(w, 200, f.list(r.URL.Query()))
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/pulls":
		list := []any{}
		if f.PR != nil {
			head := f.PR["head"].(map[string]any)
			if r.URL.Query().Get("head") == "kensac:"+head["ref"].(string) &&
				(r.URL.Query().Get("state") == "all" || r.URL.Query().Get("state") == f.PR["state"]) {
				list = append(list, f.PR)
			}
		}
		f.write(w, 200, list)
	case r.Method == "POST" && r.URL.Path == "/repos/kensac/orbit/pulls":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if f.PR != nil && f.PR["state"] == "open" && f.PR["head"].(map[string]any)["ref"] == b["head"] {
			f.write(w, 422, map[string]string{"message": "A pull request already exists for kensac:" + b["head"].(string) + "."})
			return
		}
		f.Created = append(f.Created, b)
		sha := Head
		if f.BranchHead != nil {
			sha = f.BranchHead(b["head"].(string))
		}
		f.PR = map[string]any{
			"number": 7, "state": "open", "draft": false, "merged": false, "mergeable_state": "clean",
			"title": b["title"], "body": b["body"], "html_url": "https://github.com/kensac/orbit/pull/7",
			"head": map[string]any{"ref": b["head"], "sha": sha, "repo": map[string]any{"full_name": "kensac/orbit"}},
			"base": map[string]any{"ref": b["base"], "repo": map[string]any{"full_name": "kensac/orbit", "default_branch": "main"}},
		}
		f.write(w, 201, f.PR)
	case (r.Method == "GET" || r.Method == "PATCH") && f.other(r.URL.Path) != nil:
		pr := f.other(r.URL.Path)
		if r.Method == "PATCH" {
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			f.Patched = append(f.Patched, b)
			patch(pr, b)
		}
		f.write(w, 200, pr)
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/pulls/7" && f.PR == nil:
		f.write(w, 404, map[string]string{"message": "Not Found"})
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/pulls/7":
		if f.BranchHead != nil && f.PR["state"] == "open" {
			// A push to the branch moves the open PR's head.
			head := f.PR["head"].(map[string]any)
			head["sha"] = f.BranchHead(head["ref"].(string))
		}
		pr := map[string]any{}
		for k, v := range f.PR {
			pr[k] = v
		}
		i := min(f.PRReads, len(f.Mergeable)-1)
		pr["mergeable"] = f.Mergeable[i]
		f.PRReads++
		f.write(w, 200, pr)
	case r.Method == "PATCH" && r.URL.Path == "/repos/kensac/orbit/pulls/7" && f.PR != nil:
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.Patched = append(f.Patched, b)
		patch(f.PR, b)
		f.write(w, 200, f.PR)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/kensac/orbit/compare/"):
		bh := strings.TrimPrefix(r.URL.Path, "/repos/kensac/orbit/compare/")
		st, ok := f.Compare[bh]
		if !ok {
			st = "diverged"
			if b, h, _ := strings.Cut(bh, "..."); b == h {
				st = "identical"
			}
		}
		files := []any{}
		for _, n := range f.CompareFiles {
			files = append(files, map[string]any{"filename": n})
		}
		f.write(w, 200, map[string]any{"status": st, "files": files})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/kensac/orbit/actions/jobs/") && strings.HasSuffix(r.URL.Path, "/logs"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/kensac/orbit/actions/jobs/"), "/logs")
		http.Redirect(w, r, f.Srv.URL+"/joblogs/"+id, http.StatusFound)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/repos/kensac/orbit/actions/jobs/") && strings.HasSuffix(r.URL.Path, "/rerun"):
		var id int64
		_, _ = fmt.Sscan(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/kensac/orbit/actions/jobs/"), "/rerun"), &id)
		if f.RerunStatus != 0 {
			f.write(w, f.RerunStatus, map[string]string{"message": "Resource not accessible by integration"})
			return
		}
		f.Reruns = append(f.Reruns, id)
		for _, cr := range f.CheckRuns {
			if n, _ := cr["id"].(int); int64(n) == id {
				cr["status"], cr["conclusion"] = "queued", nil
			}
		}
		w.WriteHeader(201)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/kensac/orbit/commits/") && strings.HasSuffix(r.URL.Path, "/check-runs") &&
		f.CommitChecks[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/kensac/orbit/commits/"), "/check-runs")] != nil:
		runs := f.CommitChecks[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/kensac/orbit/commits/"), "/check-runs")]
		f.write(w, 200, map[string]any{"total_count": len(runs), "check_runs": runs})
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/branches/main":
		f.write(w, 200, map[string]any{"name": "main", "protected": f.Protection != nil, "protection": f.Protection})
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/rules/branches/main":
		f.write(w, 200, f.Rules)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/kensac/orbit/branches/"):
		f.write(w, 200, map[string]any{"name": strings.TrimPrefix(r.URL.Path, "/repos/kensac/orbit/branches/"), "protected": false})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/kensac/orbit/rules/branches/"):
		f.write(w, 200, []any{})
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/commits/"+f.checkedSHA()+"/check-runs":
		f.write(w, 200, map[string]any{"total_count": len(f.CheckRuns), "check_runs": f.CheckRuns})
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/commits/"+f.checkedSHA()+"/status":
		f.write(w, 200, map[string]any{"statuses": f.Statuses})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/kensac/orbit/commits/") && strings.HasSuffix(r.URL.Path, "/status"):
		f.write(w, 200, map[string]any{"statuses": []any{}})
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/repos/kensac/orbit/issues/") && strings.HasSuffix(r.URL.Path, "/comments"):
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		var n int
		_, _ = fmt.Sscan(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/kensac/orbit/issues/"), "/comments"), &n)
		b["number"] = n
		f.Comments = append(f.Comments, b)
		f.write(w, 201, map[string]any{"id": 1000 + len(f.Comments), "html_url": fmt.Sprintf("https://github.com/kensac/orbit/issues/%d#issuecomment-%d", n, 1000+len(f.Comments))})
	case r.Method == "PUT" && r.URL.Path == "/repos/kensac/orbit/pulls/7/merge":
		_ = json.NewDecoder(r.Body).Decode(&f.Merged)
		if f.Merged["sha"] != f.PR["head"].(map[string]any)["sha"] {
			f.write(w, 409, map[string]string{"message": "Head branch was modified"})
			return
		}
		f.PR["state"], f.PR["merged"], f.PR["merge_commit_sha"] = "closed", true, f.MergeSHA
		f.write(w, 200, map[string]any{"sha": f.MergeSHA, "merged": true})
	default:
		f.write(w, 404, map[string]string{"message": "Not Found"})
	}
}

// list is GET pulls without a head filter: PR and Others whose state
// matches (open by default, or all), by number descending, one page.
func (f *Server) list(q url.Values) []any {
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	var all []map[string]any
	if f.PR != nil {
		all = append(all, f.PR)
	}
	for _, pr := range f.Others {
		all = append(all, pr)
	}
	num := func(pr map[string]any) int {
		var n int
		_, _ = fmt.Sscan(fmt.Sprint(pr["number"]), &n)
		return n
	}
	sort.Slice(all, func(i, j int) bool { return num(all[i]) > num(all[j]) })
	var match []any
	for _, pr := range all {
		if state == "all" || pr["state"] == state {
			match = append(match, pr)
		}
	}
	per, page := 30, 1
	_, _ = fmt.Sscan(q.Get("per_page"), &per)
	_, _ = fmt.Sscan(q.Get("page"), &page)
	lo, hi := min((page-1)*per, len(match)), min(page*per, len(match))
	out := match[lo:hi]
	if out == nil {
		out = []any{}
	}
	return out
}

// other is the PR of Others a path names, or nil.
func (f *Server) other(path string) map[string]any {
	var n int
	if _, err := fmt.Sscanf(path, "/repos/kensac/orbit/pulls/%d", &n); err != nil || n == 7 ||
		path != fmt.Sprintf("/repos/kensac/orbit/pulls/%d", n) {
		return nil
	}
	return f.Others[n]
}

// patch applies a PATCH body to a PR as GitHub does: base names the base
// branch, every other field replaces its value.
func patch(pr map[string]any, b map[string]string) {
	for k, v := range b {
		if k == "base" {
			pr["base"].(map[string]any)["ref"] = v
			continue
		}
		pr[k] = v
	}
}

// checkedSHA is the commit whose checks the fake reports: the PR's head,
// or Head when there is no PR.
func (f *Server) checkedSHA() string {
	if f.PR != nil {
		if sha, _ := f.PR["head"].(map[string]any)["sha"].(string); sha != "" {
			return sha
		}
	}
	return Head
}

func (f *Server) validJWT(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&Key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iat, Exp int64
		Iss      string
	}
	if json.Unmarshal(b, &c) != nil {
		return false
	}
	now := time.Now().Unix()
	return c.Iss == "42" && c.Iat <= now && c.Exp > now && c.Exp-c.Iat <= 600
}
