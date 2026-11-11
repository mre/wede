package collabdoc

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

func TestDecodeRoom(t *testing.T) {
	// base64url-encoded path decodes back to the path.
	enc := base64.RawURLEncoding.EncodeToString([]byte("src/main.go"))
	if got := decodeRoom(enc); got != "src/main.go" {
		t.Errorf("decodeRoom(%q) = %q, want src/main.go", enc, got)
	}
	// A raw path (not valid base64url — has '/' and '.') falls back unchanged.
	if got := decodeRoom("src/main.go"); got != "src/main.go" {
		t.Errorf("raw path should pass through, got %q", got)
	}
}

func TestLoadDocWithBase64Room(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewDiskPersistence(dir)
	room := base64.RawURLEncoding.EncodeToString([]byte("src/main.go"))

	update, err := p.LoadDoc(room)
	if err != nil || len(update) == 0 {
		t.Fatalf("LoadDoc(base64 room): err=%v len=%d", err, len(update))
	}
	d := crdt.New()
	crdt.ApplyUpdateV1(d, update, nil) //nolint:errcheck
	if got := d.GetText(contentField).ToString(); got != "package main" {
		t.Fatalf("materialized = %q, want %q", got, "package main")
	}
}

func TestDiskPersistenceLoadSeedsFromFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("file body"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewDiskPersistence(dir)

	update, err := p.LoadDoc("a.txt")
	if err != nil {
		t.Fatalf("LoadDoc: %v", err)
	}
	if len(update) == 0 {
		t.Fatal("expected a non-empty update for an existing file")
	}

	// A fresh doc seeded from the update must materialize the file's text.
	d := crdt.New()
	if err := crdt.ApplyUpdateV1(d, update, nil); err != nil {
		t.Fatalf("ApplyUpdateV1: %v", err)
	}
	if got := d.GetText(contentField).ToString(); got != "file body" {
		t.Fatalf("materialized = %q, want %q", got, "file body")
	}
}

func TestDiskPersistenceLoadSubdir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewDiskPersistence(dir)

	update, err := p.LoadDoc("src/main.go")
	if err != nil || len(update) == 0 {
		t.Fatalf("LoadDoc subdir: err=%v len=%d", err, len(update))
	}
	d := crdt.New()
	crdt.ApplyUpdateV1(d, update, nil) //nolint:errcheck
	if got := d.GetText(contentField).ToString(); got != "package main" {
		t.Errorf("subdir materialized = %q", got)
	}
}

func TestDiskPersistenceMissingFileEmptyDoc(t *testing.T) {
	p := NewDiskPersistence(t.TempDir())
	update, err := p.LoadDoc("does-not-exist.txt")
	if err != nil {
		t.Fatalf("LoadDoc: %v", err)
	}
	if update != nil {
		t.Errorf("missing file should seed empty (nil update), got %d bytes", len(update))
	}
}

func TestDiskPersistenceTraversalBlocked(t *testing.T) {
	p := NewDiskPersistence(t.TempDir())
	for _, bad := range []string{"../../etc/passwd", "..", "", "."} {
		update, err := p.LoadDoc(bad)
		if err != nil {
			t.Fatalf("LoadDoc(%q): %v", bad, err)
		}
		if update != nil {
			t.Errorf("LoadDoc(%q) should be blocked/empty, got %d bytes", bad, len(update))
		}
	}
}

func TestDiskPersistenceStoreUpdateNoopWithoutProvider(t *testing.T) {
	p := NewDiskPersistence(t.TempDir())
	if err := p.StoreUpdate("a.txt", []byte{1, 2, 3}); err != nil {
		t.Errorf("StoreUpdate without provider should be a no-op nil, got %v", err)
	}
}

// fakeProvider serves a fixed doc per room for write-back tests.
type fakeProvider struct{ docs map[string]*crdt.Doc }

func (f *fakeProvider) GetDoc(room string) *crdt.Doc { return f.docs[room] }

// docWith returns a crdt.Doc whose "content" text is s.
func docWith(s string) *crdt.Doc {
	d := crdt.New()
	t := d.GetText(contentField)
	d.Transact(func(txn *crdt.Transaction) { t.Insert(txn, 0, s, nil) })
	return d
}

func TestWriteBackMaterializesToDisk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewDiskPersistence(dir)
	p.SetProvider(&fakeProvider{docs: map[string]*crdt.Doc{"a.txt": docWith("new content")}})

	// flush is the synchronous core of the debounced write-back.
	p.flush("a.txt")

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new content" {
		t.Fatalf("file = %q, want %q", got, "new content")
	}
}

func TestWriteBackCreatesSubdirsAndNewFile(t *testing.T) {
	dir := t.TempDir()
	p := NewDiskPersistence(dir)
	p.SetProvider(&fakeProvider{docs: map[string]*crdt.Doc{"pkg/new.go": docWith("package pkg")}})

	p.flush("pkg/new.go")

	got, err := os.ReadFile(filepath.Join(dir, "pkg", "new.go"))
	if err != nil {
		t.Fatalf("expected new file written: %v", err)
	}
	if string(got) != "package pkg" {
		t.Errorf("new file = %q", got)
	}
}

func TestStopFlushesPending(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewDiskPersistence(dir)
	p.SetProvider(&fakeProvider{docs: map[string]*crdt.Doc{"a.txt": docWith("edited")}})

	// Schedule a debounced flush, then Stop should flush it synchronously.
	if err := p.StoreUpdate("a.txt", nil); err != nil {
		t.Fatal(err)
	}
	p.Stop()

	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "edited" {
		t.Fatalf("after Stop file = %q, want %q", got, "edited")
	}

	// After Stop, further StoreUpdate is a no-op (no panic, returns nil).
	if err := p.StoreUpdate("a.txt", nil); err != nil {
		t.Errorf("StoreUpdate after Stop: %v", err)
	}
}

type delayedDocProvider struct {
	doc     *crdt.Doc
	entered chan struct{}
	release chan struct{}
}

func (p *delayedDocProvider) GetDoc(string) *crdt.Doc {
	close(p.entered)
	<-p.release
	return p.doc
}

func TestFinalFlushSupersedesPendingWrite(t *testing.T) {
	for _, stopping := range []bool{false, true} {
		name := "timer"
		if stopping {
			name = "stop"
		}
		t.Run(name, func(t *testing.T) {
			p := NewDiskPersistence(t.TempDir())
			p.debounce = time.Hour
			prov := &delayedDocProvider{doc: docWith("old"), entered: make(chan struct{}), release: make(chan struct{})}
			p.SetProvider(prov)
			if err := p.StoreUpdate("a.txt", nil); err != nil {
				t.Fatal(err)
			}
			p.mu.Lock()
			pending := p.timers["a.txt"]
			p.mu.Unlock()
			done := make(chan struct{})
			go func() {
				defer close(done)
				if stopping {
					p.Stop()
				} else {
					p.flushPending("a.txt", pending, false)
				}
			}()
			<-prov.entered
			if err := p.FlushDoc("a.txt", docWith("final")); err != nil {
				t.Fatal(err)
			}
			close(prov.release)
			<-done
			assertDiskText(t, p, "final")
			p.Stop()
		})
	}
}

func TestFinalFlushPreservesPathContainment(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewDiskPersistence(root)
	for _, room := range []string{"../escape.txt", base64.RawURLEncoding.EncodeToString([]byte("../escape.txt"))} {
		if err := p.FlushDoc(room, docWith("nope")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("final flush escaped root: %v", err)
	}
	if err := p.FlushDoc(base64.RawURLEncoding.EncodeToString([]byte("src/a.txt")), docWith("safe")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "src/a.txt"))
	if err != nil || string(data) != "safe" {
		t.Fatalf("nested write = %q, err=%v", data, err)
	}
}

func TestFinalFlushReturnsWriteError(t *testing.T) {
	p := NewDiskPersistence(t.TempDir())
	if err := os.WriteFile(filepath.Join(p.root, "blocked"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.FlushDoc("blocked/a.txt", docWith("text")); err == nil {
		t.Fatal("expected final persistence error")
	}
}

func TestWriteBackTraversalBlocked(t *testing.T) {
	dir := t.TempDir()
	p := NewDiskPersistence(dir)
	p.SetProvider(&fakeProvider{docs: map[string]*crdt.Doc{"../escape.txt": docWith("nope")}})
	p.flush("../escape.txt")
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); err == nil {
		t.Error("write-back escaped the room root")
	}
}
