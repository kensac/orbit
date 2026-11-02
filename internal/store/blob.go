package store

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// blobSlice is how much of a stored file one query reads: a serving request holds one slice in memory, not the file (at most MaxUploadBytes).
const blobSlice = 1 << 20

// The tables whose content column a Blob reads, as fixed SQL: the table name is never a parameter.
const (
	blobUpload   = `SELECT substring(content FROM $2 FOR $3) FROM uploads WHERE id = $1`
	blobArtifact = `SELECT substring(content FROM $2 FOR $3) FROM artifacts WHERE id = $1`
)

// Blob reads one stored file's content in slices of blobSlice bytes, one query each, as an io.ReadSeeker, so http.ServeContent can send it and answer a Range request with only the slices it covers. Stored content is never updated, so slices read by separate queries are of one value.
type Blob struct {
	s     *Store
	ctx   context.Context
	query string
	id    string
	size  int64
	off   int64
	buf   []byte // the slice starting at bufAt
	bufAt int64
}

func (s *Store) blob(ctx context.Context, query, id string, size int64) *Blob {
	return &Blob{s: s, ctx: ctx, query: query, id: id, size: size}
}

// Size is the content's length in bytes.
func (b *Blob) Size() int64 { return b.size }

// Read implements io.Reader.
func (b *Blob) Read(p []byte) (int, error) {
	if b.off >= b.size {
		return 0, io.EOF
	}
	if b.off < b.bufAt || b.off >= b.bufAt+int64(len(b.buf)) {
		n := min(int64(blobSlice), b.size-b.off)
		var chunk []byte
		// substring counts from 1.
		if err := b.s.Pool.QueryRow(b.ctx, b.query, b.id, b.off+1, n).Scan(&chunk); err != nil {
			return 0, err
		}
		if int64(len(chunk)) != n {
			return 0, fmt.Errorf("stored file %s: read %d bytes at %d, want %d", b.id, len(chunk), b.off, n)
		}
		b.buf, b.bufAt = chunk, b.off
	}
	n := copy(p, b.buf[b.off-b.bufAt:])
	b.off += int64(n)
	return n, nil
}

// Seek implements io.Seeker.
func (b *Blob) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += b.off
	case io.SeekEnd:
		offset += b.size
	default:
		return 0, errors.New("blob: invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("blob: negative position")
	}
	b.off = offset
	return offset, nil
}
