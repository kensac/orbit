package boxkeep

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kensac/orbit/internal/store"
)

// SyncOnce puts orbit's skills library into each running box's shared
// profile, and the MCP servers orbit manages (box.mcp_servers) into each of
// its accounts, when either changed since this instance last did (every
// box once after a start).
func (k *Keeper) SyncOnce(ctx context.Context) {
	if k.synced == nil {
		k.synced = map[string]string{}
	}
	c, boxes, sts := k.running(ctx)
	if len(boxes) == 0 {
		return
	}
	skills, digest, err := k.skills(ctx)
	if err != nil {
		k.Log.Warn("skills sync: read the library", "err", err)
	}
	var want map[string]json.RawMessage
	if k.MCPServers != nil {
		want = k.MCPServers()
	}
	if want == nil {
		want = map[string]json.RawMessage{}
	}
	mb, _ := json.Marshal(want)
	sum := sha256.Sum256(mb)
	mcpHash := hex.EncodeToString(sum[:])
	for _, b := range boxes {
		if err == nil && k.synced["skills/"+b.Agent] != digest {
			tb, terr := skillsTar(skills)
			if terr == nil {
				terr = c.SyncSkills(ctx, b.Agent, bytes.NewReader(tb))
			}
			if terr != nil {
				k.Log.Warn("skills sync", "agent", b.Agent, "err", terr)
			} else {
				k.synced["skills/"+b.Agent] = digest
				k.Log.Info("skills synced", "agent", b.Agent, "skills", len(skills))
			}
		}
		as, aerr := k.Store.ListAccounts(ctx, b.ID)
		if aerr != nil {
			k.Log.Warn("mcp sync: accounts", "agent", b.Agent, "err", aerr)
			continue
		}
		for _, a := range as {
			if !slices.Contains(sts[b.Agent].Accounts, a.Slot) || k.synced["mcp/"+a.Slot] == mcpHash {
				continue
			}
			if err := c.SyncAccountMCP(ctx, b.Agent, a.Slot, want); err != nil {
				k.Log.Warn("mcp sync", "agent", b.Agent, "account", a.ID, "err", err)
				continue
			}
			k.synced["mcp/"+a.Slot] = mcpHash
			k.Log.Info("mcp servers synced", "agent", b.Agent, "account", a.ID, "servers", len(want))
		}
	}
}

// skills is the library's active skills in full, with a digest of their
// versions.
func (k *Keeper) skills(ctx context.Context) ([]store.Skill, string, error) {
	list, err := k.Store.ListSkills(ctx, "")
	if err != nil {
		return nil, "", err
	}
	h := sha256.New()
	names := make([]string, 0, len(list))
	for _, s := range list {
		_, _ = fmt.Fprintf(h, "%s@%d:%s\n", s.Name, s.Version, s.SHA256)
		names = append(names, s.Name)
	}
	full, err := k.Store.SkillsByName(ctx, names)
	return full, hex.EncodeToString(h.Sum(nil)), err
}

// skillsTar is each skill as <name>/SKILL.md and <name>/<file>.
func skillsTar(skills []store.Skill) ([]byte, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	put := func(name string, content []byte) error {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o644}); err != nil {
			return err
		}
		_, err := tw.Write(content)
		return err
	}
	for _, s := range skills {
		if err := put(s.Name+"/SKILL.md", []byte(s.Content)); err != nil {
			return nil, err
		}
		for _, f := range s.Files {
			if err := store.ValidSkillPath(f.Path); err != nil {
				return nil, err
			}
			if err := put(s.Name+"/"+f.Path, f.Content); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
