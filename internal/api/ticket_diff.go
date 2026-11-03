package api

import (
	"math"
	"net/http"

	"github.com/kensac/orbit/internal/diff"
	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/workspace"
)

// A ticket's PR as diff files (docs/api.md, Tickets, A ticket's diff): the
// same shape as a run's changes, from GitHub's list of the PR's files and
// their patches, read with a token for the ticket's repository alone
// (contents and pull_requests read) that is revoked after the request.

// pullStatus maps GitHub's file status to a diff file's.
var pullStatus = map[string]string{
	"added": diff.Added, "removed": diff.Deleted, "modified": diff.Modified, "renamed": diff.Renamed,
	"copied": diff.Added, "changed": diff.Mode, "unchanged": diff.Modified,
}

// pullFileDiff is one PR file as a diff file. A file GitHub gives no patch
// for is binary when it changed no lines and too large to show (truncated)
// when it did; a rename without changes has no lines either.
func pullFileDiff(f github.PullFile, maxLines int) (diff.File, error) {
	d := diff.File{Path: f.Filename, Status: pullStatus[f.Status], Additions: f.Additions, Deletions: f.Deletions, Hunks: []diff.Hunk{}}
	if d.Status == "" {
		d.Status = diff.Modified
	}
	if f.PreviousFilename != "" {
		old := f.PreviousFilename
		d.OldPath = &old
	}
	if f.Patch == "" {
		switch {
		case f.Additions+f.Deletions > 0:
			d.Truncated = true
		case d.Status != diff.Renamed && d.Status != diff.Mode:
			d.Binary = true
		}
		return d, nil
	}
	hunks, _, _, truncated, err := diff.ParsePatch(f.Patch, maxLines)
	if err != nil {
		return d, err
	}
	if hunks != nil {
		d.Hunks = hunks
	}
	d.Truncated = truncated
	return d, nil
}

// ticketDiff is GET /tickets/{id}/diff?path=.
func (s *Server) ticketDiff(w http.ResponseWriter, r *http.Request) {
	only := ""
	if r.URL.Query().Has("path") {
		p, err := workspace.CleanPath(r.URL.Query().Get("path"))
		if err != nil || p == "" {
			writeErr(w, http.StatusBadRequest, "path: not a clean path of a file in the repository")
			return
		}
		only = p
	}
	t, err := s.Store.GetTicket(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if t.Repo == nil || t.PRNumber == nil {
		writeErr(w, http.StatusConflict, "the ticket has no PR yet")
		return
	}
	d := s.Engine.Current().Config.Deployment
	app, err := github.NewApp(d.GitHub, s.Engine.Env, s.Engine.Client)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	tok, err := app.Token(r.Context(), *t.Repo, github.ReadPullDiff)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "github: "+err.Error())
		return
	}
	defer func() {
		if err := tok.Revoke(r.Context()); err != nil {
			s.Log.Warn("revoke the ticket diff token", "ticket", t.ID, "err", err)
		}
	}()
	gh := tok.Client()
	pr, err := gh.PR(r.Context(), *t.Repo, *t.PRNumber)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "github: "+err.Error())
		return
	}
	limit := diff.MaxFiles + 1
	if only != "" {
		limit = github.MaxPullFiles
	}
	list, err := gh.PullFiles(r.Context(), *t.Repo, *t.PRNumber, limit)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "github: "+err.Error())
		return
	}
	files := []diff.File{}
	truncated, bytes := false, 0
	for _, f := range list {
		if only != "" && f.Filename != only {
			continue
		}
		if len(files) == diff.MaxFiles {
			truncated = true
			break
		}
		maxLines := diff.MaxFileLines
		if only != "" {
			maxLines = math.MaxInt
		}
		df, err := pullFileDiff(f, maxLines)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "github: "+f.Filename+": "+err.Error())
			return
		}
		if n := df.Bytes(); bytes+n > diff.MaxBytes {
			df.Hunks, df.Truncated, truncated = []diff.Hunk{}, true, true
		} else {
			bytes += n
		}
		files = append(files, df)
	}
	if only != "" && len(files) == 0 {
		writeErr(w, http.StatusNotFound, "the PR does not change "+only)
		return
	}
	respond(w, map[string]any{"ticket_id": t.ID, "repo": *t.Repo, "pr": *t.PRNumber,
		"base":  map[string]string{"ref": pr.Base.Ref, "sha": pr.Base.SHA},
		"head":  map[string]string{"ref": pr.Head.Ref, "sha": pr.Head.SHA},
		"files": files, "truncated": truncated}, nil)
}
