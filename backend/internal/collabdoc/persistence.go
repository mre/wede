package collabdoc

import (
	"encoding/base64"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/reearth/ygo/crdt"
)

// decodeRoom maps a provider room name back to a room-relative file path. The
// frontend encodes the path as base64url (RawURLEncoding) so it is a single,
// slash-free, URL-safe token for the y-websocket client. Real file paths contain
// characters outside the base64url alphabet ('/', '.'), so a failed decode
// safely falls back to treating the name as a raw path (handy for curl/testing).
func decodeRoom(room string) string {
	if b, err := base64.RawURLEncoding.DecodeString(room); err == nil {
		return string(b)
	}
	if b, err := base64.URLEncoding.DecodeString(room); err == nil {
		return string(b)
	}
	return room
}

// DocProvider is the subset of ygo's provider/websocket Server that the
// persistence layer needs: fetch the live doc for a room. Defined as an interface
// so this package stays decoupled from the provider package.
type DocProvider interface {
	GetDoc(name string) *crdt.Doc
}

// defaultDebounce is how long write-back waits after the last edit before
// flushing materialized text to disk.
const defaultDebounce = 600 * time.Millisecond

// DiskPersistence is a ygo provider PersistenceAdapter backed by files on disk.
// The provider "room" name is a file's room-relative path.
//
//   - LoadDoc seeds a document from the file (so a new doc materializes the
//     file's current text).
//   - StoreUpdate fires on each incremental edit; rather than persist opaque
//     updates, we debounce and write the doc's full materialized text back to the
//     file via the DocProvider (GetDoc -> YText "content" -> ToString). Writes are
//     atomic (temp + rename).
//
// External disk changes (e.g. git checkout, a terminal edit) re-seeding an open
// doc is a separate, later slice — it needs care to avoid feedback loops with the
// write-back below.
type DiskPersistence struct {
	root     string
	debounce time.Duration

	mu       sync.Mutex
	provider DocProvider
	timers   map[string]*pendingWrite
	stopped  bool
}

// Each scheduled write has an identity so a cancelled callback cannot flush a
// replacement document after the old room has been retired.
type pendingWrite struct {
	timer *time.Timer
}

// NewDiskPersistence returns an adapter rooted at a room's workspace directory.
func NewDiskPersistence(root string) *DiskPersistence {
	return &DiskPersistence{
		root:     root,
		debounce: defaultDebounce,
		timers:   make(map[string]*pendingWrite),
	}
}

// SetProvider wires the live document source (the ygo Server). Until set,
// StoreUpdate is a no-op.
func (p *DiskPersistence) SetProvider(dp DocProvider) {
	p.mu.Lock()
	p.provider = dp
	p.mu.Unlock()
}

// safeJoin resolves rel under root, rejecting any path that escapes root.
func safeJoin(root, rel string) (string, bool) {
	if rel == "" || rel == "." {
		return "", false
	}
	full := filepath.Join(root, rel)
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

// LoadDoc returns the stored state for a document (room == relative file path),
// or (nil, nil) when there is nothing to seed (new/missing file, traversal, or a
// directory) — the provider then starts an empty doc.
func (p *DiskPersistence) LoadDoc(room string) ([]byte, error) {
	full, ok := safeJoin(p.root, decodeRoom(room))
	if !ok {
		return nil, nil
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, nil
	}

	doc := crdt.New()
	t := doc.GetText(contentField)
	doc.Transact(func(txn *crdt.Transaction) {
		t.Insert(txn, 0, string(data), nil)
	})
	return crdt.EncodeStateAsUpdateV1(doc, nil), nil
}

// StoreUpdate schedules a debounced write-back of the room's materialized text.
func (p *DiskPersistence) StoreUpdate(room string, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.provider == nil {
		return nil
	}
	if pending, ok := p.timers[room]; ok {
		pending.timer.Reset(p.debounce)
		return nil
	}
	pending := &pendingWrite{}
	p.timers[room] = pending
	pending.timer = time.AfterFunc(p.debounce, func() { p.flushPending(room, pending, false) })
	return nil
}

// flush is the synchronous form used by tests and explicit write-back.
func (p *DiskPersistence) flush(room string) {
	p.flushPending(room, nil, false)
}

func (p *DiskPersistence) flushPending(room string, pending *pendingWrite, stopping bool) {
	p.mu.Lock()
	prov := p.provider
	p.mu.Unlock()
	if prov == nil {
		return
	}
	// Never call the provider under p.mu: lifecycle flushes and room loading
	// may need that lock. Validate the timer again after resolving the doc.
	doc := prov.GetDoc(room)
	p.mu.Lock()
	defer p.mu.Unlock()
	if (p.stopped && !stopping) || (pending != nil && p.timers[room] != pending) {
		return
	}
	delete(p.timers, room)
	if err := p.materialize(room, doc); err != nil {
		log.Printf("collabdoc: write-back %q: %v", room, err)
	}
}

// FlushDoc is the provider's final persistence barrier. It receives the actual
// retiring document, not a name lookup that can resolve to nil or a new room.
// It must still write after Stop: Workspace.shutdown stops debounce timers
// before closing sockets, and accepted edits may arrive in that interval.
func (p *DiskPersistence) FlushDoc(room string, doc *crdt.Doc) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pending := p.timers[room]; pending != nil {
		pending.timer.Stop()
		delete(p.timers, room)
	}
	return p.materialize(room, doc)
}

// materialize is called under p.mu, serializing timer, Stop, and lifecycle writes
// so an older snapshot cannot finish writing after the final flush.
func (p *DiskPersistence) materialize(room string, doc *crdt.Doc) error {
	if doc == nil {
		return nil
	}
	full, ok := safeJoin(p.root, decodeRoom(room))
	if !ok {
		return nil
	}
	return writeAtomic(full, []byte(doc.GetText(contentField).ToString()))
}

// Stop disables debouncing and synchronously flushes pending live documents.
// The provider must subsequently be shut down: its FlushDoc barrier persists
// accepted edits not yet delivered to StoreUpdate, including edits during Stop.
func (p *DiskPersistence) Stop() {
	p.mu.Lock()
	p.stopped = true
	pending := make(map[string]*pendingWrite, len(p.timers))
	for room, write := range p.timers {
		write.timer.Stop()
		pending[room] = write
	}
	p.mu.Unlock()

	for room, write := range pending {
		p.flushPending(room, write, true)
	}
}

// writeAtomic writes data to path via a temp file + rename, creating parent dirs.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".wede-collab-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
