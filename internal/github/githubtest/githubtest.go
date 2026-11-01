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
	"net/http"
	"net/http/httptest"
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
	if r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
		f.write(w, 400, map[string]string{"message": "no api version"})
		return
	}
	auth := r.Header.Get("Authorization")
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
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/pulls/7":
		pr := map[string]any{}
		for k, v := range f.PR {
			pr[k] = v
		}
		i := min(f.PRReads, len(f.Mergeable)-1)
		pr["mergeable"] = f.Mergeable[i]
		f.PRReads++
		f.write(w, 200, pr)
	case r.Method == "PATCH" && r.URL.Path == "/repos/kensac/orbit/pulls/7":
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.Patched = append(f.Patched, b)
		f.PR["body"] = b["body"]
		f.write(w, 200, f.PR)
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/branches/main":
		f.write(w, 200, map[string]any{"name": "main", "protected": f.Protection != nil, "protection": f.Protection})
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/rules/branches/main":
		f.write(w, 200, f.Rules)
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/commits/"+Head+"/check-runs":
		f.write(w, 200, map[string]any{"total_count": len(f.CheckRuns), "check_runs": f.CheckRuns})
	case r.Method == "GET" && r.URL.Path == "/repos/kensac/orbit/commits/"+Head+"/status":
		f.write(w, 200, map[string]any{"statuses": f.Statuses})
	case r.Method == "PUT" && r.URL.Path == "/repos/kensac/orbit/pulls/7/merge":
		_ = json.NewDecoder(r.Body).Decode(&f.Merged)
		if f.Merged["sha"] != f.PR["head"].(map[string]any)["sha"] {
			f.write(w, 409, map[string]string{"message": "Head branch was modified"})
			return
		}
		f.write(w, 200, map[string]any{"sha": "2222", "merged": true})
	default:
		f.write(w, 404, map[string]string{"message": "Not Found"})
	}
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
