package sandboxd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/kensac/orbit/internal/sandbox"
)

const claudePluginInstallScript = `import hashlib,io,json,os,re,secrets,stat,sys,tarfile
root,plugin_id,wanted,bundle_sha=sys.argv[1:]
raw=sys.stdin.buffer.read(33554433)
if len(raw)>33554432: raise ValueError("plugin archive exceeds its size limit")
if hashlib.sha256(raw).hexdigest()!=bundle_sha: raise ValueError("plugin archive digest does not match")
seg=r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}"
if not re.fullmatch(seg+r"@"+seg,plugin_id): raise ValueError("invalid plugin ID")
name,market=plugin_id.split("@",1)
def safe_segment(s): return re.fullmatch(seg,s) is not None and ".." not in s
def digest(files,version):
    h=hashlib.sha256(); h.update(("orbit-claude-plugin-v1\0"+plugin_id+"\0"+version+"\0").encode())
    for f in files:
        h.update(f["path"].encode()+b"\0"+str(f["mode"]).encode()+b"\0"+str(f["bytes"]).encode()+b"\0"+f["sha256"].encode()+b"\0")
    return h.hexdigest()
def no_link(path):
    if os.path.islink(path): raise ValueError("plugin target contains a symlink")
def mkdir_chain(base,parts):
    no_link(base)
    if not os.path.isdir(base): raise ValueError("account config directory is missing")
    current=base
    for part in parts:
        current=os.path.join(current,part)
        try: os.mkdir(current,0o700)
        except FileExistsError: pass
        no_link(current)
        if not os.path.isdir(current): raise ValueError("plugin target path is not a directory")
    return current
def scan_tree(base):
    found=[]; total=0
    for here,dirs,names in os.walk(base,followlinks=False):
        for d in dirs:
            path=os.path.join(here,d); no_link(path)
            if not os.path.isdir(path): raise ValueError("plugin directory is not regular")
        for n in names:
            path=os.path.join(here,n); no_link(path)
            st=os.lstat(path)
            if not stat.S_ISREG(st.st_mode): raise ValueError("plugin file is not regular")
            rel=os.path.relpath(path,base).replace(os.sep,"/")
            fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
            with os.fdopen(fd,"rb") as stream: data=stream.read(33554433)
            if len(data)>33554432: raise ValueError("plugin file exceeds its size limit")
            total+=len(data)
            if total>33554432 or len(found)>=2000: raise ValueError("installed plugin exceeds its file or byte limit")
            found.append({"path":rel,"mode":0o755 if st.st_mode&0o111 else 0o644,"bytes":len(data),"sha256":hashlib.sha256(data).hexdigest()})
    found.sort(key=lambda f:f["path"])
    return found
with tarfile.open(fileobj=io.BytesIO(raw),mode="r:") as tar:
    members=tar.getmembers()
    if not members or len(members)>2000: raise ValueError("plugin archive has no files or exceeds its file limit")
    prefix=None; version=None; payload=[]; total=0; seen=set()
    for m in members:
        if not m.isfile() or m.issym() or m.islnk(): raise ValueError("plugin archive contains a non-regular file")
        parts=m.name.split("/")
        if len(parts)<6 or parts[0]!="plugin" or parts[1]!=market or parts[2]!=name or parts[3] in ("",".","..") or not safe_segment(parts[3]): raise ValueError("plugin archive path does not match its ID")
        if prefix is None: version=parts[3]; prefix=parts[:4]
        if parts[:4]!=prefix or len(m.name)>1024 or any(p in ("",".","..") or len(p.encode())>255 for p in parts[4:]) or "\\" in m.name or "\0" in m.name: raise ValueError("plugin archive has an unsafe path")
        rel="/".join(parts[4:])
        base=rel.rsplit("/",1)[-1].lower()
        if rel in seen or base in (".mcp.json",".claude.json",".credentials.json","settings.json","auth.json") or base.startswith(".env"): raise ValueError("plugin archive has a duplicate path or settings or credential file")
        seen.add(rel)
        mode=0o755 if m.mode&0o111 else 0o644
        if m.mode not in (0o644,0o755): raise ValueError("plugin archive has a noncanonical mode")
        f=tar.extractfile(m)
        if f is None: raise ValueError("plugin archive entry has no content")
        data=f.read(33554433-total)
        if len(data)!=m.size: raise ValueError("plugin archive entry size does not match")
        total+=len(data)
        if total>33554432: raise ValueError("plugin archive exceeds its size limit")
        payload.append({"path":rel,"mode":mode,"bytes":len(data),"sha256":hashlib.sha256(data).hexdigest(),"data":data})
    payload.sort(key=lambda f:f["path"])
if digest(payload,version)!=wanted: raise ValueError("plugin payload digest does not match operator pin")
manifest=next((f for f in payload if f["path"]==".claude-plugin/plugin.json"),None)
if manifest is None: raise ValueError("plugin manifest is missing")
m=json.loads(manifest["data"])
if m.get("name")!=name or m.get("version")!=version: raise ValueError("plugin manifest identity does not match")
root=os.path.abspath(root)
plugins=mkdir_chain(root,["plugins"])
cache=mkdir_chain(plugins,["cache",market,name])
target=os.path.join(cache,version)
stage=os.path.join(plugins,".orbit-plugin-"+secrets.token_hex(12))
existing_target=os.path.lexists(target)
if existing_target:
    no_link(target)
    if not os.path.isdir(target): raise ValueError("plugin target exists and is not a directory")
else:
    os.mkdir(stage,0o700)
def write_file(base,rel,data,mode):
    parts=rel.split("/"); parent=mkdir_chain(base,parts[:-1]) if len(parts)>1 else base
    no_link(parent)
    path=os.path.join(parent,parts[-1])
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,mode)
    with os.fdopen(fd,"wb") as out: out.write(data); out.flush(); os.fsync(out.fileno())
try:
    if not existing_target:
        for f in payload: write_file(stage,f["path"],f["data"],f["mode"])
        # Re-read the complete staged tree before making it visible.
        verify=scan_tree(stage)
        if digest(verify,version)!=wanted: raise ValueError("staged plugin digest changed before install")
        os.rename(stage,target)
except:
    import shutil
    if not existing_target: shutil.rmtree(stage,ignore_errors=True)
    raise
# Verify the installed bytes again before enabling the plugin.
installed=scan_tree(target)
if digest(installed,version)!=wanted: raise ValueError("installed plugin digest does not match operator pin")
def read_json(path,default):
    if os.path.islink(path): raise ValueError("account plugin metadata cannot be a symlink")
    if not os.path.exists(path): return default
    with open(path,encoding="utf-8") as stream: return json.load(stream)
def write_json(path,value):
    parent=os.path.dirname(path); no_link(parent)
    tmp=os.path.join(parent,".orbit-plugin-"+secrets.token_hex(12))
    fd=os.open(tmp,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,"w",encoding="utf-8") as stream:
        json.dump(value,stream,ensure_ascii=False,sort_keys=True,indent=2); stream.write("\n"); stream.flush(); os.fsync(stream.fileno())
    os.replace(tmp,path)
settings_path=os.path.join(root,"settings.json")
settings=read_json(settings_path,{})
if not isinstance(settings,dict): raise ValueError("account settings must be an object")
enabled=settings.setdefault("enabledPlugins",{})
if not isinstance(enabled,dict): raise ValueError("enabledPlugins must be an object")
installed_path=os.path.join(plugins,"installed_plugins.json")
index=read_json(installed_path,{"version":2,"plugins":{}})
records=index.setdefault("plugins",{})
if not isinstance(records,dict): raise ValueError("installed plugin index is invalid")
existing=records.setdefault(plugin_id,[])
if not isinstance(existing,list): raise ValueError("installed plugin records are invalid")
install_path=target
record=next((r for r in existing if isinstance(r,dict) and r.get("scope")=="user" and r.get("installPath")==install_path and r.get("version")==version),None)
if record is None:
    now=__import__("datetime").datetime.now(__import__("datetime").timezone.utc).isoformat()
    existing.append({"scope":"user","installPath":install_path,"version":version,"installedAt":now,"lastUpdated":now})
enabled[plugin_id]=True
write_json(installed_path,index)
write_json(settings_path,settings)
print(json.dumps({"plugin":plugin_id,"version":version,"digest":wanted,"files":len(payload)},separators=(",",":")))`

func (s *Server) claudePluginPut(w http.ResponseWriter, r *http.Request) {
	s.boxMu.Lock()
	defer s.boxMu.Unlock()
	s.claudePluginMu.Lock()
	defer s.claudePluginMu.Unlock()
	c, name, slot, ok := s.accountBox(w, r)
	if !ok {
		return
	}
	pluginID := r.PathValue("plugin")
	if !sandbox.ValidClaudePluginID(pluginID) {
		writeError(w, http.StatusBadRequest, "plugin ID must be name@marketplace")
		return
	}
	bundleDigest := r.Header.Get("X-Orbit-Plugin-SHA256")
	archiveDigest := r.Header.Get("X-Orbit-Bundle-SHA256")
	if !validConfigDigest(bundleDigest) || !validConfigDigest(archiveDigest) {
		writeError(w, http.StatusBadRequest, "plugin and bundle digests must be lowercase sha256 values")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, sandbox.MaxClaudeBundleBytes)
	archive, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "plugin archive exceeds its size limit")
		return
	}
	if len(archive) == 0 {
		writeError(w, http.StatusBadRequest, "plugin archive is empty")
		return
	}
	h := sha256.Sum256(archive)
	if hex.EncodeToString(h[:]) != archiveDigest {
		writeError(w, http.StatusBadRequest, "plugin archive digest does not match its bytes")
		return
	}
	stdout, stderr, code, err := s.boxRun(r.Context(), c.ID, []string{"python3", "-c", claudePluginInstallScript, "/home/agent/.claude-accounts/" + slot, pluginID, bundleDigest, archiveDigest}, nil, archive)
	if err != nil || code != 0 {
		writeError(w, http.StatusBadGateway, scriptError("plugin install", code, stderr))
		return
	}
	var result struct {
		Plugin  string `json:"plugin"`
		Version string `json:"version"`
		Digest  string `json:"digest"`
		Files   int    `json:"files"`
	}
	dec := json.NewDecoder(strings.NewReader(string(stdout)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil || result.Plugin != pluginID || result.Digest != bundleDigest || result.Files < 1 {
		writeError(w, http.StatusBadGateway, "plugin install returned an invalid result")
		return
	}
	s.log.Info("sandboxd: Claude plugin imported", "box", name, "account", slot, "plugin", pluginID, "version", result.Version, "digest", result.Digest, "files", result.Files)
	writeJSON(w, http.StatusOK, result)
}
