package collabdoc

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ywebsocket "github.com/reearth/ygo/provider/websocket"
	ysync "github.com/reearth/ygo/sync"
)

func lifecycleServer(t *testing.T) (*DiskPersistence, *ywebsocket.Server, string) {
	t.Helper()
	return lifecycleServerWithAdapter(t, func(p *DiskPersistence) ywebsocket.PersistenceAdapter { return p })
}

func lifecycleServerWithAdapter(t *testing.T, wrap func(*DiskPersistence) ywebsocket.PersistenceAdapter) (*DiskPersistence, *ywebsocket.Server, string) {
	t.Helper()
	p := NewDiskPersistence(t.TempDir())
	// Make the disconnect-before-debounce condition deterministic, not a race
	// against a short wall-clock timeout on a busy machine.
	p.debounce = time.Hour
	s := ywebsocket.NewServerWithPersistence(wrap(p))
	p.SetProvider(s)
	h := httptest.NewServer(s)
	t.Cleanup(func() {
		p.Stop() // Match Workspace.shutdown's existing ordering.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		h.Close()
	})
	return p, s, "ws" + strings.TrimPrefix(h.URL, "http") + "/YS50eHQ"
}

func connectDocument(t *testing.T, url string) (*gws.Conn, *crdt.Doc) {
	t.Helper()
	c, _, err := gws.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	doc := crdt.New()
	// The provider sends sync step 1, sync step 2, then awareness.
	for i := 0; i < 3; i++ {
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		dec := encoding.NewDecoder(data)
		kind, err := dec.ReadVarUint()
		if err != nil {
			t.Fatal(err)
		}
		if kind == 0 {
			if _, err := ysync.ApplySyncMessage(doc, dec.RemainingBytes(), nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	return c, doc
}

func sendAcceptedEdit(t *testing.T, c *gws.Conn, doc *crdt.Doc, text string) {
	t.Helper()
	var update []byte
	unsub := doc.OnUpdate(func(b []byte, _ any) { update = b })
	txt := doc.GetText(contentField)
	doc.Transact(func(tx *crdt.Transaction) {
		txt.Insert(tx, 0, text, nil)
	})
	unsub()
	wire := append([]byte{0}, ysync.EncodeUpdate(update)...)
	if err := c.WriteMessage(gws.BinaryMessage, wire); err != nil {
		t.Fatal(err)
	}
	// Application ping is processed after the update on the same read loop.
	// Its pong proves acceptance before we close the last connection.
	if err := c.WriteMessage(gws.BinaryMessage, []byte{9}); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, pong, err := c.ReadMessage()
	if err != nil || len(pong) != 1 || pong[0] != 10 {
		t.Fatalf("acceptance barrier: pong=%v err=%v", pong, err)
	}
}

func assertDiskText(t *testing.T, p *DiskPersistence, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(p.root, "a.txt"))
	if err != nil || string(data) != want {
		t.Fatalf("disk text = %q, err=%v; want %q", data, err, want)
	}
}

func TestLastPeerDisconnectFlushesAcceptedEdit(t *testing.T) {
	p, s, url := lifecycleServer(t)
	unloaded := make(chan struct{}, 2)
	s.OnUnloadDocument = func(context.Context, string) { unloaded <- struct{}{} }
	c, doc := connectDocument(t, url)
	sendAcceptedEdit(t, c, doc, "accepted")
	if got := s.GetDoc("YS50eHQ").GetText(contentField).ToString(); got != "accepted" {
		t.Fatalf("server did not accept edit: %q", got)
	}
	c.Close()
	select {
	case <-unloaded:
	case <-time.After(3 * time.Second):
		t.Fatal("last peer did not unload")
	}
	assertDiskText(t, p, "accepted")
	_, reopened := connectDocument(t, url)
	if got := reopened.GetText(contentField).ToString(); got != "accepted" {
		t.Fatalf("reopened document = %q", got)
	}
}

func TestWorkspaceShutdownFlushesAcceptedEditAfterStop(t *testing.T) {
	p, s, url := lifecycleServer(t)
	c, doc := connectDocument(t, url)
	sendAcceptedEdit(t, c, doc, "first")
	// Workspace.shutdown calls Stop before Server.Shutdown. Connections are
	// still live in that interval; final persistence must include those edits.
	p.Stop()
	sendAcceptedEdit(t, c, doc, "last ")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	assertDiskText(t, p, "last first")
}

// Hold the actual final persistence barrier open to exercise reconnect and
// shutdown ordering without racing the disk write's duration.
type gatedFlush struct {
	*DiskPersistence
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedFlush) FlushDoc(room string, doc *crdt.Doc) error {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.DiskPersistence.FlushDoc(room, doc)
}

func TestReconnectWaitsForRetiringDocumentFlush(t *testing.T) {
	var gate *gatedFlush
	p, s, url := lifecycleServerWithAdapter(t, func(p *DiskPersistence) ywebsocket.PersistenceAdapter {
		gate = &gatedFlush{DiskPersistence: p, entered: make(chan struct{}), release: make(chan struct{})}
		return gate
	})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(release)
	c, doc := connectDocument(t, url)
	sendAcceptedEdit(t, c, doc, "accepted")
	c.Close()
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("final flush not reached")
	}
	if s.GetDoc("YS50eHQ") != nil {
		t.Fatal("retiring document is still exposed as live")
	}
	type dialResult struct {
		conn *gws.Conn
		err  error
	}
	result := make(chan dialResult, 1)
	go func() {
		dialer := gws.Dialer{HandshakeTimeout: 3 * time.Second}
		conn, _, err := dialer.Dial(url, nil)
		result <- dialResult{conn, err}
	}()
	select {
	case r := <-result:
		if r.conn != nil {
			r.conn.Close()
		}
		t.Fatalf("reconnect completed before final persistence: %v", r.err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case r := <-result:
		if r.err != nil {
			t.Fatal(r.err)
		}
		t.Cleanup(func() { r.conn.Close() })
	case <-time.After(3 * time.Second):
		t.Fatal("reconnect blocked after flush")
	}
	assertDiskText(t, p, "accepted")
	_, reopened := connectDocument(t, url)
	if got := reopened.GetText(contentField).ToString(); got != "accepted" {
		t.Fatalf("reopened document = %q", got)
	}
}

func TestShutdownWaitsForFinalPersistence(t *testing.T) {
	for _, state := range []string{"active", "already-retiring"} {
		t.Run(state, func(t *testing.T) {
			var gate *gatedFlush
			p, s, url := lifecycleServerWithAdapter(t, func(p *DiskPersistence) ywebsocket.PersistenceAdapter {
				gate = &gatedFlush{DiskPersistence: p, entered: make(chan struct{}), release: make(chan struct{})}
				return gate
			})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate.release) }) }
			t.Cleanup(release)
			c, doc := connectDocument(t, url)
			sendAcceptedEdit(t, c, doc, "accepted")
			if state == "already-retiring" {
				c.Close()
				select {
				case <-gate.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("disconnect did not reach final flush")
				}
			}
			p.Stop()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- s.Shutdown(ctx) }()
			select {
			case <-gate.entered:
			case <-ctx.Done():
				t.Fatal("final flush not reached")
			}
			select {
			case err := <-result:
				t.Fatalf("shutdown returned before final persistence: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			release()
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			assertDiskText(t, p, "accepted")
		})
	}
}

func TestLifecycleFlushesPeerlessDocument(t *testing.T) {
	for _, action := range []string{"close-room", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			p, s, _ := lifecycleServer(t)
			if err := s.Apply(context.Background(), "YS50eHQ", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
				txt := doc.GetText(contentField)
				transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, "injected", nil) })
			}); err != nil {
				t.Fatal(err)
			}
			if action == "close-room" {
				if err := s.CloseRoom("YS50eHQ", false); err != nil {
					t.Fatal(err)
				}
			} else {
				p.Stop()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := s.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
			}
			assertDiskText(t, p, "injected")
		})
	}
}

func TestUneditedDocumentDoesNotCreateFileOnDisconnect(t *testing.T) {
	p, s, url := lifecycleServer(t)
	unloaded := make(chan struct{}, 1)
	s.OnUnloadDocument = func(context.Context, string) { unloaded <- struct{}{} }
	c, _ := connectDocument(t, url)
	c.Close()
	select {
	case <-unloaded:
	case <-time.After(3 * time.Second):
		t.Fatal("last peer did not unload")
	}
	if _, err := os.Stat(filepath.Join(p.root, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("opening an unedited document created a file: %v", err)
	}
}
