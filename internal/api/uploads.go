package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/store"
)

// UploadURLTTL is how long a signed upload URL works: long enough for a
// browser to send 64 MiB on a slow connection, short enough that a leaked
// one is soon useless. Each URL also uploads at most once (its nonce).
const UploadURLTTL = 15 * time.Minute

// multipartOverhead is room for a multipart body's boundaries and headers
// beyond the file itself.
const multipartOverhead = 1 << 20

// uploadSigner keys signed upload URLs from the operator token, so either
// instance of a rollout verifies the other's, and rotating the token voids
// every outstanding URL.
func (s *Server) uploadKey() []byte {
	m := hmac.New(sha256.New, []byte(s.Token))
	m.Write([]byte("orbit upload url"))
	return m.Sum(nil)
}

// uploadSig signs the values a signed upload URL carries, in a fixed order.
func (s *Server) uploadSig(v url.Values) string {
	m := hmac.New(sha256.New, s.uploadKey())
	for _, k := range []string{"c", "n", "t", "b", "e", "a", "k"} {
		m.Write([]byte(k + "=" + v.Get(k) + "\x00"))
	}
	return hex.EncodeToString(m.Sum(nil))
}

// uploadReply is the shape every upload route answers with.
func uploadReply(u store.Upload) map[string]any {
	return map[string]any{"upload": u}
}

// createUpload is POST /conversations/{id}/uploads: one multipart file
// (field "file", or the first file part).
func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.GetConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxUploadBytes+multipartOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "want multipart/form-data with a file: "+err.Error())
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "no file in the form")
			return
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if part.FileName() == "" {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part, store.MaxUploadBytes+1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(b) > store.MaxUploadBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, "a file is at most "+strconv.Itoa(store.MaxUploadBytes)+" bytes")
			return
		}
		mt := part.Header.Get("Content-Type")
		if mt == "" || mt == "application/octet-stream" {
			mt = http.DetectContentType(b)
		}
		u, err := s.Store.CreateUpload(r.Context(), store.NewUpload{ConversationID: c.ID, Name: part.FileName(),
			MediaType: mt, Content: b, Author: actor(r, "")})
		respondStatus(w, http.StatusCreated, uploadReply(u), err)
		return
	}
}

// uploadURL is POST /conversations/{id}/uploads/url: a signed URL the
// browser PUTs one file to directly, for files larger than its proxy takes.
func (s *Server) uploadURL(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.GetConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	var body struct {
		Name      string `json:"name"`
		MediaType string `json:"media_type"`
		Bytes     int64  `json:"bytes"`
	}
	if !decode(w, r, 16<<10, &body, bodyRequired) {
		return
	}
	if body.Bytes < 0 || body.Bytes > store.MaxUploadBytes {
		writeErr(w, http.StatusBadRequest, "bytes must be 0 to "+strconv.Itoa(store.MaxUploadBytes))
		return
	}
	mt := strings.TrimSpace(body.MediaType)
	if mt == "" {
		mt = "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(mt); err != nil {
		writeErr(w, http.StatusBadRequest, "media_type: "+err.Error())
		return
	}
	if len(s.Token) == 0 {
		writeErr(w, http.StatusServiceUnavailable, "signed uploads need ORBIT_API_TOKEN")
		return
	}
	base := s.publicURL()
	if base == "" {
		writeErr(w, http.StatusServiceUnavailable, "signed uploads need public_url in orbit.yaml")
		return
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	exp := time.Now().Add(UploadURLTTL).Truncate(time.Second)
	v := url.Values{}
	v.Set("c", c.ID)
	v.Set("n", store.UploadName(body.Name))
	v.Set("t", mt)
	v.Set("b", strconv.FormatInt(body.Bytes, 10))
	v.Set("e", strconv.FormatInt(exp.Unix(), 10))
	v.Set("a", actor(r, ""))
	v.Set("k", hex.EncodeToString(nonce))
	v.Set("s", s.uploadSig(v))
	respond(w, map[string]any{
		"url": base + "/api/v1/uploads/signed?" + v.Encode(), "method": http.MethodPut,
		"headers": map[string]string{"Content-Type": mt}, "expires_at": exp, "max_bytes": body.Bytes,
	}, nil)
}

// signedUpload is PUT /api/v1/uploads/signed: the body is the file. The
// signature stands in for a token: it names one conversation, one name and
// type, a size bound, an expiry and the person it was issued to.
func (s *Server) signedUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(s.Token) == 0 || !hmac.Equal([]byte(s.uploadSig(q)), []byte(q.Get("s"))) {
		writeErr(w, http.StatusForbidden, "the upload link is not valid")
		return
	}
	exp, _ := strconv.ParseInt(q.Get("e"), 10, 64)
	if time.Now().Unix() > exp {
		writeErr(w, http.StatusForbidden, "the upload link has expired")
		return
	}
	limit, _ := strconv.ParseInt(q.Get("b"), 10, 64)
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if int64(len(b)) > limit {
		writeErr(w, http.StatusRequestEntityTooLarge, "the file is larger than the link allows ("+q.Get("b")+" bytes)")
		return
	}
	u, err := s.Store.CreateUpload(r.Context(), store.NewUpload{ConversationID: q.Get("c"), Name: q.Get("n"),
		MediaType: q.Get("t"), Content: b, Author: q.Get("a"), Nonce: q.Get("k")})
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	respondStatus(w, http.StatusCreated, uploadReply(u), err)
}

// getUpload is GET /conversations/{id}/uploads/{upload}: the file itself,
// inline for an image, a download otherwise, never rendered as a page.
func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) {
	u, b, err := s.Store.GetUpload(r.Context(), r.PathValue("id"), r.PathValue("upload"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	serveFile(w, u.Name, u.MediaType, b)
}

// serveFile sends a stored file: images inline (the chat shows them),
// everything else as an attachment, with nosniff and a CSP that runs
// nothing, so a file a person or an agent wrote is never a page on orbit's
// origin.
func serveFile(w http.ResponseWriter, name, mediaType string, b []byte) {
	disp := "attachment"
	mt, _, _ := mime.ParseMediaType(mediaType)
	if strings.HasPrefix(mt, "image/") && mt != "image/svg+xml" {
		disp = "inline"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}
