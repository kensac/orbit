package store_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// contentSends counts the queries that carried a []byte argument of one length: how often a file's content crossed the wire.
type contentSends struct {
	mu   sync.Mutex
	size int
	n    int
}

func (c *contentSends) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	for _, a := range d.Args {
		if b, ok := a.([]byte); ok && len(b) == c.size {
			c.mu.Lock()
			c.n++
			c.mu.Unlock()
		}
	}
	return ctx
}

func (c *contentSends) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *contentSends) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// traced is a second store on s's database whose queries tr sees.
func traced(t *testing.T, s *store.Store, tr pgx.QueryTracer) *store.Store {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(s.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}
}

func TestUploadNameCollisionsSendContentOnce(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", Title: "files"})
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("orbit"), 200_000)
	sends := &contentSends{size: len(content)}
	ts := traced(t, s, sends)
	for i, want := range []string{"report.pdf", "report (2).pdf", "report (3).pdf"} {
		before := sends.count()
		u, err := ts.CreateUpload(ctx, store.NewUpload{ConversationID: r.ConversationID, Name: "report.pdf", MediaType: "application/pdf", Content: content})
		if err != nil {
			t.Fatal(err)
		}
		if u.Name != want {
			t.Fatalf("upload %d named %q, want %q", i+1, u.Name, want)
		}
		if got := sends.count() - before; got != 1 {
			t.Fatalf("upload %d sent its content %d times, want once", i+1, got)
		}
	}

	// A used nonce is refused before the content is sent again.
	n := store.NewUpload{ConversationID: r.ConversationID, Name: "once.bin", Content: content, Nonce: "k1"}
	if _, err := ts.CreateUpload(ctx, n); err != nil {
		t.Fatal(err)
	}
	before := sends.count()
	if _, err := ts.CreateUpload(ctx, n); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second use of a nonce = %v, want ErrConflict", err)
	}
	if got := sends.count() - before; got != 0 {
		t.Fatalf("a refused nonce sent the content %d times", got)
	}

	// An unknown conversation is ErrNotFound, also without sending it.
	before = sends.count()
	if _, err := ts.CreateUpload(ctx, store.NewUpload{ConversationID: "00000000-0000-0000-0000-000000000000", Name: "x", Content: content}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown conversation = %v", err)
	}
	if got := sends.count() - before; got != 0 {
		t.Fatalf("an unknown conversation sent the content %d times", got)
	}
}

func TestUploadsConcurrentSameNameGetDistinctNames(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", Title: "files"})
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	names := make(chan string, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			u, err := s.CreateUpload(ctx, store.NewUpload{ConversationID: r.ConversationID, Name: "a.txt", Content: []byte("x")})
			if err != nil {
				t.Error(err)
				return
			}
			names <- u.Name
		})
	}
	wg.Wait()
	close(names)
	seen := map[string]bool{}
	for name := range names {
		if seen[name] {
			t.Fatalf("two uploads named %q", name)
		}
		seen[name] = true
	}
	if len(seen) != n || !seen["a.txt"] || !seen["a (8).txt"] {
		t.Fatalf("names = %v", seen)
	}
}

func TestUploadReadsInSlices(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", Title: "files"})
	if err != nil {
		t.Fatal(err)
	}
	// Several slices and a partial last one, of bytes that differ by position.
	content := make([]byte, 5<<20+12345)
	for i := range content {
		content[i] = byte(i * 7 / 3)
	}
	u, err := s.CreateUpload(ctx, store.NewUpload{ConversationID: r.ConversationID, Name: "video.mp4", MediaType: "video/mp4", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	got, b, err := s.OpenUpload(ctx, r.ConversationID, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bytes != int64(len(content)) || b.Size() != int64(len(content)) || got.SHA256 != store.SHA256(content) {
		t.Fatalf("upload = %+v, size %d", got, b.Size())
	}
	all, err := io.ReadAll(b)
	if err != nil || !bytes.Equal(all, content) {
		t.Fatalf("read whole: %d bytes, %v", len(all), err)
	}
	// A read from the middle, across a slice boundary.
	off := int64(3<<20 - 10)
	if _, err := b.Seek(off, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	part := make([]byte, 100)
	if _, err := io.ReadFull(b, part); err != nil || !bytes.Equal(part, content[off:off+100]) {
		t.Fatalf("read at %d: %v", off, err)
	}
	if _, _, err := s.OpenUpload(ctx, r.ConversationID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown upload = %v", err)
	}
}
