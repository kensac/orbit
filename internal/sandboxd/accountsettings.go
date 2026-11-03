package sandboxd

// accountSettingsScript keeps the shared profile settings as defaults while
// persisting per-account overrides in the account volume. This lets one
// account enable a plugin without enabling it for every account that shares
// the profile.
const accountSettingsScript = `import json,os,secrets,sys
shared=sys.argv[1]
account=sys.argv[2]
if os.path.islink(account) or not os.path.isdir(account): raise ValueError("account directory is not real")
def parse(path, missing_ok=False):
    try:
        with open(path,encoding="utf-8") as f: raw=f.read(1048577)
    except FileNotFoundError:
        if missing_ok: return {}
        raise
    if len(raw)>1048576: raise ValueError("settings exceed 1 MiB")
    # Claude settings permit JavaScript-style comments and trailing commas.
    out=[]; i=0; state="value"; quote=False; escape=False
    while i<len(raw):
        c=raw[i]
        if quote:
            out.append(c)
            if escape: escape=False
            elif c=="\\": escape=True
            elif c=='"': quote=False
            i+=1; continue
        if c=='"': quote=True; out.append(c); i+=1; continue
        if c=="/" and i+1<len(raw) and raw[i+1]=="/":
            out.extend("  "); i+=2
            while i<len(raw) and raw[i] not in "\r\n": out.append(" "); i+=1
            continue
        if c=="/" and i+1<len(raw) and raw[i+1]=="*":
            out.extend("  "); i+=2; closed=False
            while i<len(raw):
                if raw[i:i+2]=="*/": out.extend("  "); i+=2; closed=True; break
                out.append("\n" if raw[i]=="\n" else " "); i+=1
            if not closed: raise ValueError("unterminated settings comment")
            continue
        out.append(c); i+=1
    text="".join(out)
    # Remove commas immediately before a closing bracket or brace outside strings.
    out=[]; i=0; quote=False; escape=False
    while i<len(text):
        c=text[i]
        if quote:
            out.append(c)
            if escape: escape=False
            elif c=="\\": escape=True
            elif c=='"': quote=False
            i+=1; continue
        if c=='"': quote=True; out.append(c); i+=1; continue
        if c==",":
            j=i+1
            while j<len(text) and text[j].isspace(): j+=1
            if j<len(text) and text[j] in "]}": i+=1; continue
        out.append(c); i+=1
    value=json.loads("".join(out))
    if not isinstance(value,dict): raise ValueError("settings must be an object")
    return value
def diff(local, base):
    if isinstance(local,dict) and isinstance(base,dict):
        out={}
        for k,v in local.items():
            if k not in base: out[k]=v
            else:
                d=diff(v,base[k])
                if d is not None: out[k]=d
        return out if out else None
    return None if local==base else local
def merge(defaults, overrides):
    if isinstance(defaults,dict) and isinstance(overrides,dict):
        out=dict(defaults)
        for k,v in overrides.items(): out[k]=merge(defaults[k],v) if k in defaults else v
        return out
    return overrides
if os.path.islink(shared) or (os.path.exists(shared) and not os.path.isfile(shared)): raise ValueError("shared settings path is not a regular file")
base=parse(shared,True)
settings=os.path.join(account,"settings.json")
link=os.path.islink(settings)
if link and os.readlink(settings)!="../../.claude/settings.json": raise ValueError("account settings link has an unexpected target")
local={} if link else parse(settings,True)
basefile=os.path.join(account,".orbit","settings-base.json")
if os.path.islink(basefile): raise ValueError("settings baseline is a symlink")
previous=parse(basefile,True) if os.path.exists(basefile) else None
overrides={} if link else (local if previous is None else (diff(local,previous) or {}))
result=merge(base,overrides)
meta=os.path.dirname(basefile)
if os.path.islink(meta) or (os.path.exists(meta) and not os.path.isdir(meta)): raise ValueError("account metadata path is not a real directory")
os.makedirs(meta,mode=0o700,exist_ok=True)
def write(path,value):
    data=(json.dumps(value,ensure_ascii=False,sort_keys=True,indent=2)+"\n").encode()
    tmp=os.path.join(os.path.dirname(path),".orbit-settings-"+secrets.token_hex(12))
    fd=os.open(tmp,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    try:
        with os.fdopen(fd,"wb") as f: f.write(data); f.flush(); os.fsync(f.fileno())
        os.replace(tmp,path)
    except:
        try: os.unlink(tmp)
        except FileNotFoundError: pass
        raise
write(settings,result)
write(basefile,base)
print(json.dumps({"keys":len(result),"overrides":len(overrides)},separators=(",",":")))`
