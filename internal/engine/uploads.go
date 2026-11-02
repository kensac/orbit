package engine

import (
	"context"
	"os"
	"path/filepath"

	"github.com/kensac/orbit/internal/store"
)

// UploadsDir is where a run finds the files its person's messages carry,
// relative to its workdir: uploads/<name>, the name unique in the
// conversation (store.CreateUpload).
const UploadsDir = "uploads"

// placedUpload is one attachment as the run sees it.
type placedUpload struct {
	Path      string
	MediaType string
	Bytes     int64
}

// placeUploads writes the attachments of the messages this run consumed into
// its workdir, and returns them by message id for the prompt. The uploads
// table is their record; a file already there from an earlier run of the
// same workspace is written again, as stored.
func (e *Engine) placeUploads(ctx context.Context, workdir string, rc *store.RunContext) (map[string][]placedUpload, error) {
	ids := make([]string, 0, len(rc.Messages))
	for _, m := range rc.Messages {
		ids = append(ids, m.ID)
	}
	att, err := e.Store.Attachments(ctx, ids)
	if err != nil || len(att) == 0 {
		return nil, err
	}
	var uploadIDs []string
	for _, list := range att {
		for _, u := range list {
			uploadIDs = append(uploadIDs, u.ID)
		}
	}
	content, err := e.Store.UploadContents(ctx, uploadIDs)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(workdir, UploadsDir), 0o700); err != nil {
		return nil, err
	}
	out := map[string][]placedUpload{}
	for mid, list := range att {
		for _, u := range list {
			rel := filepath.Join(UploadsDir, store.UploadName(u.Name))
			if err := os.WriteFile(filepath.Join(workdir, rel), content[u.ID], 0o600); err != nil {
				return nil, err
			}
			out[mid] = append(out[mid], placedUpload{Path: rel, MediaType: u.MediaType, Bytes: u.Bytes})
		}
	}
	return out, nil
}
