package workspace

import (
	"bytes"
	"sort"
)

// The operations of a path change, as the change stream names them
// (docs/api.md, Workspace, The change stream).
const (
	OpCreate  = "create"
	OpModify  = "modify"
	OpTrash   = "trash"
	OpRestore = "restore"
	OpRename  = "rename"
)

// PathChange is one path that differs between two snapshots: every kind of
// entry, every path under a directory that came or went.
type PathChange struct {
	Op   string
	Path string
	// From is a rename's old path.
	From string
	Kind string
	// Version is a file's version after the change ("" for a trash and for
	// directories and symlinks).
	Version string
}

// Changes lists every path that differs from before to after (nil is an
// empty tree), by path. A file that leaves one path and arrives at another
// with the same content is a rename; a path whose kind changed is a trash
// and a create. Changes reads no file twice: versions are the snapshots'
// own, computed once.
func Changes(before, after *Snapshot) []PathChange {
	entries := func(s *Snapshot) map[string]*Entry {
		if s == nil {
			return nil
		}
		return s.entries
	}
	a, b := entries(before), entries(after)
	var out, added []PathChange
	var removed []*Entry
	for p, y := range b {
		if p == "" {
			continue
		}
		x, ok := a[p]
		switch {
		case ok && x.Kind == y.Kind:
			if x.Kind == KindFile && (x.Mode != y.Mode || !bytes.Equal(x.data, y.data)) ||
				x.Kind == KindSymlink && x.Target != y.Target {
				out = append(out, change(after, OpModify, y))
			}
			continue
		case ok:
			removed = append(removed, x)
		}
		added = append(added, change(after, OpCreate, y))
	}
	for p, x := range a {
		if _, ok := b[p]; !ok && p != "" {
			removed = append(removed, x)
		}
	}
	sort.Slice(added, func(i, j int) bool { return added[i].Path < added[j].Path })
	sort.Slice(removed, func(i, j int) bool { return removed[i].Path < removed[j].Path })
	// Removed files by content, for renames.
	byVersion := map[string][]*Entry{}
	for _, x := range removed {
		if x.Kind == KindFile {
			v := before.VersionOf(x)
			byVersion[v] = append(byVersion[v], x)
		}
	}
	renamed := map[string]bool{}
	for _, c := range added {
		if c.Kind == KindFile {
			if list := byVersion[c.Version]; len(list) > 0 {
				c.Op, c.From = OpRename, list[0].Path
				byVersion[c.Version] = list[1:]
				renamed[c.From] = true
			}
		}
		out = append(out, c)
	}
	for _, x := range removed {
		if x.Kind == KindFile && renamed[x.Path] {
			continue
		}
		out = append(out, PathChange{Op: OpTrash, Path: x.Path, Kind: x.Kind})
	}
	// By path; at one path the trash of the old kind comes first.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Op == OpTrash && out[j].Op != OpTrash
	})
	return out
}

func change(s *Snapshot, op string, e *Entry) PathChange {
	c := PathChange{Op: op, Path: e.Path, Kind: e.Kind}
	if e.Kind == KindFile {
		c.Version = s.VersionOf(e)
	}
	return c
}
