package sandboxd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/store"
)

const claudeConfigStatusScript = `import hashlib,json,os,stat,sys
root=sys.argv[1]
def skillname(n): return n.isascii() and 1<=len(n)<=63 and 'a'<=n[0]<='z' and all(('a'<=c<='z') or ('0'<=c<='9') or c=='-' for c in n)
def valid(p):
    if p == "CLAUDE.md": return True
    a=p.split("/")
    return len(a)>=3 and a[0]=="skills" and skillname(a[1]) and (a[2:]==["SKILL.md"] or (a[2:] and "/".join(a[2:])!="SKILL.md" and "\\" not in p and "\0" not in p and all(x not in ("", ".", "..") for x in a[2:])))
def scan(base):
    if os.path.islink(base) or not os.path.isdir(base): raise ValueError("Claude profile is not a real directory")
    out=[]
    def add(p):
        if not valid(p): raise ValueError("unsupported Claude config path: "+p)
        f=os.path.join(base,*p.split("/"))
        st=os.lstat(f)
        if not stat.S_ISREG(st.st_mode): raise ValueError("unsupported Claude config file: "+p)
        h=hashlib.sha256()
        fd=os.open(f,os.O_RDONLY|os.O_NOFOLLOW)
        with os.fdopen(fd,"rb") as stream:
            while True:
                b=stream.read(65536)
                if not b: break
                h.update(b)
        out.append({"path":p,"bytes":st.st_size,"sha256":h.hexdigest()})
        if len(out)>2000 or sum(x["bytes"] for x in out)>33554432: raise ValueError("Claude config exceeds its file or byte limit")
    ca=os.path.join(base,"CLAUDE.md")
    if os.path.lexists(ca): add("CLAUDE.md")
    skills=os.path.join(base,"skills")
    if os.path.lexists(skills):
        if os.path.islink(skills) or not os.path.isdir(skills): raise ValueError("skills is not a real directory")
        for skill in os.scandir(skills):
            if not skillname(skill.name) or skill.is_symlink() or not skill.is_dir(follow_symlinks=False): continue
            marker=os.path.join(skill.path,"SKILL.md")
            if os.path.islink(marker) or not os.path.isfile(marker): continue
            for here,dirs,files in os.walk(skill.path,followlinks=False):
                for d in dirs:
                    if os.path.islink(os.path.join(here,d)): raise ValueError("symlink in skills directory")
                for f in files:
                    p=os.path.relpath(os.path.join(here,f),base).replace(os.sep,"/")
                    add(p)
    out.sort(key=lambda x:x["path"])
    return out
print(json.dumps(scan(root),separators=(",",":")))`

const claudeConfigInstallScript = `import hashlib,json,os,secrets,shutil,stat,sys
stage=sys.argv[1]
root=sys.argv[2]
request=json.load(sys.stdin)
expected=request["expected"]
wanted=request["files"]
def skillname(n): return n.isascii() and 1<=len(n)<=63 and 'a'<=n[0]<='z' and all(('a'<=c<='z') or ('0'<=c<='9') or c=='-' for c in n)
def valid(p):
    if p == "CLAUDE.md": return True
    a=p.split("/")
    return len(a)>=3 and a[0]=="skills" and skillname(a[1]) and (a[2:]==["SKILL.md"] or (a[2:] and "\\" not in p and "\0" not in p and all(x not in ("", ".", "..") for x in a[2:]) and "/".join(a[2:])!="SKILL.md"))
def scan(base):
    if os.path.islink(base) or not os.path.isdir(base): raise ValueError("Claude profile is not a real directory")
    out=[]
    def add(p):
        if not valid(p): raise ValueError("unsupported Claude config path: "+p)
        f=os.path.join(base,*p.split("/")); st=os.lstat(f)
        if not stat.S_ISREG(st.st_mode): raise ValueError("unsupported Claude config file: "+p)
        h=hashlib.sha256()
        fd=os.open(f,os.O_RDONLY|os.O_NOFOLLOW)
        with os.fdopen(fd,"rb") as stream:
            while True:
                b=stream.read(65536)
                if not b: break
                h.update(b)
        out.append({"path":p,"bytes":st.st_size,"sha256":h.hexdigest()})
        if len(out)>2000 or sum(x["bytes"] for x in out)>33554432: raise ValueError("Claude config exceeds its file or byte limit")
    ca=os.path.join(base,"CLAUDE.md")
    if os.path.lexists(ca): add("CLAUDE.md")
    skills=os.path.join(base,"skills")
    if os.path.lexists(skills):
        if os.path.islink(skills) or not os.path.isdir(skills): raise ValueError("skills is not a real directory")
        for skill in os.scandir(skills):
            if not skillname(skill.name) or skill.is_symlink() or not skill.is_dir(follow_symlinks=False): continue
            marker=os.path.join(skill.path,"SKILL.md")
            if os.path.islink(marker) or not os.path.isfile(marker): continue
            for here,dirs,files in os.walk(skill.path,followlinks=False):
                for d in dirs:
                    if os.path.islink(os.path.join(here,d)): raise ValueError("symlink in skills directory")
                for f in files: add(os.path.relpath(os.path.join(here,f),base).replace(os.sep,"/"))
    out.sort(key=lambda x:x["path"])
    return out
def digest(files):
    h=hashlib.sha256()
    for f in files:
        h.update(f["path"].encode()+b"\0"+str(f["bytes"]).encode()+b"\0"+f["sha256"].encode()+b"\0")
    return h.hexdigest()
def open_dir(basefd,parts,create):
    fd=os.dup(basefd)
    try:
        for part in parts:
            try: nxt=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
            except FileNotFoundError:
                if not create: raise
                os.mkdir(part,0o755,dir_fd=fd)
                nxt=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
            os.close(fd); fd=nxt
        return fd
    except:
        os.close(fd); raise
def copy_one(p,entry):
    parts=p.split("/"); srcfd=os.open(stage,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    try:
        pfd=open_dir(srcfd,parts[:-1],False)
        try: sfd=os.open(parts[-1],os.O_RDONLY|os.O_NOFOLLOW,dir_fd=pfd)
        finally: os.close(pfd)
    finally: os.close(srcfd)
    try:
        dstroot=os.open(root,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
        try: dfd=open_dir(dstroot,parts[:-1],True)
        finally: os.close(dstroot)
        try:
            try:
                st=os.stat(parts[-1],dir_fd=dfd,follow_symlinks=False)
                if not stat.S_ISREG(st.st_mode): raise ValueError("target is not a regular file: "+p)
            except FileNotFoundError: pass
            tmp=".orbit-sync-"+secrets.token_hex(12)
            d=os.open(tmp,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600,dir_fd=dfd)
            try:
                h=hashlib.sha256(); size=0
                while True:
                    b=os.read(sfd,65536)
                    if not b: break
                    h.update(b); size+=len(b)
                    view=memoryview(b)
                    while view:
                        n=os.write(d,view); view=view[n:]
                if size!=entry["bytes"] or h.hexdigest()!=entry["sha256"]:
                    os.unlink(tmp,dir_fd=dfd); raise ValueError("staged Claude config changed: "+p)
                os.fchmod(d,0o644); os.fsync(d)
            finally: os.close(d)
            os.rename(tmp,parts[-1],src_dir_fd=dfd,dst_dir_fd=dfd)
        finally: os.close(dfd)
    finally: os.close(sfd)
rootfd=os.open(root,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
os.close(rootfd)
before=scan(root)
if digest(before)!=expected:
    print(json.dumps({"conflict":True,"files":before},separators=(",",":"))); sys.exit(4)
source=scan(stage)
if source!=wanted: raise ValueError("staged bundle does not match its validated manifest")
for f in source: copy_one(f["path"],f)
print(json.dumps({"conflict":False,"files":scan(root)},separators=(",",":")))`

const claudeConfigStageScript = `import os,sys
p="/tmp/orbit-claude-sync-"+sys.argv[1]
os.mkdir(p,0o700)`

const claudeConfigCleanupScript = `import shutil,sys
shutil.rmtree("/tmp/orbit-claude-sync-"+sys.argv[1])`

func (s *Server) claudeConfigGet(w http.ResponseWriter, r *http.Request) {
	s.boxMu.Lock()
	defer s.boxMu.Unlock()
	c, _, _, ok := s.accountBox(w, r)
	if !ok {
		return
	}
	status, err := s.readClaudeConfig(r.Context(), c.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Claude config: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) claudeConfigPut(w http.ResponseWriter, r *http.Request) {
	s.boxMu.Lock()
	defer s.boxMu.Unlock()
	s.claudeConfigMu.Lock()
	defer s.claudeConfigMu.Unlock()
	c, _, slot, ok := s.accountBox(w, r)
	if !ok {
		return
	}
	expected := r.Header.Get("If-Match")
	bundleDigest := r.Header.Get("X-Orbit-Bundle-SHA256")
	if !validConfigDigest(expected) || !validConfigDigest(bundleDigest) {
		writeError(w, http.StatusBadRequest, "If-Match and X-Orbit-Bundle-SHA256 must be lowercase sha256 values")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, sandbox.MaxClaudeBundleBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "Claude config bundle exceeds its size limit")
		return
	}
	archiveHash := sha256.Sum256(raw)
	if hex.EncodeToString(archiveHash[:]) != bundleDigest {
		writeError(w, http.StatusBadRequest, "Claude config bundle digest does not match its bytes")
		return
	}
	clean, incoming, err := sandbox.ParseClaudeConfigTar(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	current, err := s.readClaudeConfig(r.Context(), c.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "read current Claude config: "+err.Error())
		return
	}
	if current.Digest != expected {
		writeClaudeConfigConflict(w, current)
		return
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "create Claude config staging id")
		return
	}
	stage := hex.EncodeToString(token[:])
	if _, stderr, code, err := s.boxRun(r.Context(), c.ID, []string{"python3", "-c", claudeConfigStageScript, stage}, nil, nil); err != nil || code != 0 {
		writeError(w, http.StatusBadGateway, scriptError("Claude config staging", code, stderr))
		return
	}
	stagePath := "/tmp/orbit-claude-sync-" + stage
	if err := s.d.putArchive(r.Context(), c.ID, stagePath, bytes.NewReader(clean)); err != nil {
		s.cleanupClaudeConfigStage(r.Context(), c.ID, stage)
		writeError(w, http.StatusBadGateway, "stage Claude config: "+err.Error())
		return
	}
	request, err := json.Marshal(struct {
		Expected string                     `json:"expected"`
		Files    []sandbox.ClaudeConfigFile `json:"files"`
	}{Expected: expected, Files: incoming.Files})
	if err != nil {
		s.cleanupClaudeConfigStage(r.Context(), c.ID, stage)
		writeError(w, http.StatusInternalServerError, "encode Claude config manifest")
		return
	}
	stdout, stderr, code, err := s.boxRun(r.Context(), c.ID, []string{"python3", "-c", claudeConfigInstallScript, stagePath, "/home/agent/.claude"}, nil, request)
	s.cleanupClaudeConfigStage(r.Context(), c.ID, stage)
	if err != nil || code != 0 {
		if code == 4 && err == nil {
			status, parseErr := parseClaudeConfigInstallResult(stdout)
			if parseErr == nil || status.Digest != "" {
				writeClaudeConfigConflict(w, status)
				return
			}
		}
		writeError(w, http.StatusBadGateway, scriptError("Claude config install", code, stderr))
		return
	}
	status, err := parseClaudeConfigInstallResult(stdout)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Claude config install: "+err.Error())
		return
	}
	s.log.Info("sandboxd: Claude config imported", "box", r.PathValue("name"), "account", slot, "files", len(incoming.Files), "digest", status.Digest)
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) readClaudeConfig(ctx context.Context, id string) (sandbox.ClaudeConfigStatus, error) {
	stdout, stderr, code, err := s.boxRun(ctx, id, []string{"python3", "-c", claudeConfigStatusScript, "/home/agent/.claude"}, nil, nil)
	if err != nil {
		return sandbox.ClaudeConfigStatus{}, err
	}
	if code != 0 {
		return sandbox.ClaudeConfigStatus{}, errors.New(scriptError("status", code, stderr))
	}
	return parseClaudeConfigStatus(stdout)
}

func parseClaudeConfigStatus(raw []byte) (sandbox.ClaudeConfigStatus, error) {
	var files []sandbox.ClaudeConfigFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&files); err != nil {
		return sandbox.ClaudeConfigStatus{}, fmt.Errorf("decode profile manifest: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return sandbox.ClaudeConfigStatus{}, errors.New("profile manifest has extra data")
	}
	if len(files) > 2000 {
		return sandbox.ClaudeConfigStatus{}, errors.New("profile manifest has too many files")
	}
	seen := map[string]bool{}
	var total int
	for _, f := range files {
		if !validClaudeConfigPath(f.Path) || f.Bytes < 0 || !validConfigDigest(f.SHA256) || seen[f.Path] {
			return sandbox.ClaudeConfigStatus{}, fmt.Errorf("invalid file in profile manifest: %q", f.Path)
		}
		seen[f.Path] = true
		total += f.Bytes
		if total > sandbox.MaxClaudeBundleBytes {
			return sandbox.ClaudeConfigStatus{}, errors.New("profile manifest exceeds the size limit")
		}
	}
	return sandbox.ClaudeConfigStatus{Digest: sandbox.ClaudeConfigDigest(files), Files: files}, nil
}

func parseClaudeConfigInstallResult(raw []byte) (sandbox.ClaudeConfigStatus, error) {
	var result struct {
		Conflict bool                       `json:"conflict"`
		Files    []sandbox.ClaudeConfigFile `json:"files"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return sandbox.ClaudeConfigStatus{}, fmt.Errorf("decode install result: %w", err)
	}
	status, err := parseClaudeConfigStatus(encodeClaudeManifest(result.Files))
	if err != nil {
		return sandbox.ClaudeConfigStatus{}, err
	}
	if result.Conflict {
		return status, errors.New("target changed during install")
	}
	return status, nil
}

func encodeClaudeManifest(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func writeClaudeConfigConflict(w http.ResponseWriter, status sandbox.ClaudeConfigStatus) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": "Claude config changed since planning", "config": status})
}

func validConfigDigest(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validClaudeConfigPath(p string) bool {
	if p == "CLAUDE.md" {
		return true
	}
	if strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return false
	}
	parts := strings.Split(p, "/")
	if len(parts) < 3 || parts[0] != "skills" || !store.SkillNameRE.MatchString(parts[1]) {
		return false
	}
	if !validSkillDir(parts[1]) {
		return false
	}
	if strings.Join(parts, "/") == "skills/"+parts[1]+"/SKILL.md" {
		return true
	}
	return store.ValidSkillPath(strings.Join(parts[2:], "/")) == nil
}

func validSkillDir(name string) bool {
	if len(name) < 1 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func (s *Server) cleanupClaudeConfigStage(ctx context.Context, id, stage string) {
	_, stderr, code, err := s.boxRun(ctx, id, []string{"python3", "-c", claudeConfigCleanupScript, stage}, nil, nil)
	if err != nil || code != 0 {
		s.log.Warn("sandboxd: Claude config staging cleanup failed", "err", strings.TrimSpace(string(stderr)))
	}
}
