package terminal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

type sizeEvent struct {
	Type     string `json:"type"`
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
	Revision uint64 `json:"revision"`
}

func decodeSize(t *testing.T, msg outbound) sizeEvent {
	t.Helper()
	var size sizeEvent
	if msg.kind != websocket.TextMessage || json.Unmarshal(msg.data, &size) != nil || size.Type != "size" {
		t.Fatalf("expected size, got kind=%d data=%q", msg.kind, msg.data)
	}
	return size
}

func geometrySession(t *testing.T) *session {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	s := newTestSession()
	s.ptmx = master
	if err := pty.Setsize(master, &pty.Winsize{Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	return s
}

func assertGeometry(t *testing.T, s *session, cols, rows int, revision uint64) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cols != cols || s.rows != rows || s.revision != revision {
		t.Fatalf("geometry %dx%d rev %d, want %dx%d rev %d", s.cols, s.rows, s.revision, cols, rows, revision)
	}
	actual, err := pty.GetsizeFull(s.ptmx)
	if err != nil {
		t.Fatal(err)
	}
	if int(actual.Cols) != cols || int(actual.Rows) != rows {
		t.Fatalf("PTY geometry: %+v", actual)
	}
}

func TestGeometryVisibleMinimumAndRetention(t *testing.T) {
	s := geometrySession(t)
	a, b := s.addSub(nil, true), s.addSub(nil, true)
	decodeSize(t, <-a.out)
	decodeSize(t, <-b.out)
	s.viewport(a, 120, 30, true)
	s.viewport(b, 25, 40, true)
	assertGeometry(t, s, 25, 30, 2) // minima are independent per axis
	s.viewport(b, 25, 10, true)
	assertGeometry(t, s, 25, 10, 3)
	s.viewport(b, 0, 0, false)
	assertGeometry(t, s, 120, 30, 4)
	// Hidden subscribers receive all canonical changes too.
	for revision := uint64(1); revision <= 4; revision++ {
		for _, sub := range []*subscriber{a, b} {
			if got := decodeSize(t, <-sub.out); got.Revision != revision {
				t.Fatalf("size: %+v", got)
			}
		}
	}
	s.viewport(a, 120, 30, true)
	assertGeometry(t, s, 120, 30, 4)
	s.viewport(b, 25, 10, true)
	s.removeSub(b)
	assertGeometry(t, s, 120, 30, 6)
	s.removeSub(a)
	assertGeometry(t, s, 120, 30, 6)
}

func TestGeometryInvalidControlsAndBoundaries(t *testing.T) {
	s := geometrySession(t)
	sub := s.addSub(nil, true)
	<-sub.out
	for _, message := range []string{
		`{"type":"viewport","cols":1,"rows":24,"visible":true}`,
		`{"type":"viewport","cols":1001,"rows":24,"visible":true}`,
		`{"type":"viewport","cols":80,"rows":0,"visible":true}`,
		`{"type":"viewport","cols":80,"rows":501,"visible":true}`,
		`{"type":"viewport","cols":-1,"rows":24,"visible":true}`,
		`{"type":"viewport","cols":65538,"rows":24,"visible":true}`,
		`{"type":"viewport","cols":2.5,"rows":24,"visible":true}`,
		`{"type":"viewport","cols":"80","rows":24,"visible":true}`,
		`{"type":"viewport","cols":80,"rows":24}`,
		`{"type":"viewport","visible":true}`,
		`{"type":"resize","cols":25,"rows":10}`,
		`not JSON`,
	} {
		if !s.handleControl(sub, []byte(message)) {
			t.Fatalf("control leaked to input: %s", message)
		}
		assertGeometry(t, s, 80, 24, 0)
	}
	if len(sub.out) != 0 {
		t.Fatal("invalid dimensions emitted a size")
	}
	s.viewport(sub, 2, 1, true)
	assertGeometry(t, s, 2, 1, 1)
	s.viewport(sub, 1000, 500, true)
	assertGeometry(t, s, 1000, 500, 2)
	s.handleControl(sub, []byte(`{"type":"viewport","visible":false}`))
	assertGeometry(t, s, 1000, 500, 2)
}

func TestGeometryResizeFailureKeepsAuthoritativeSize(t *testing.T) {
	s := geometrySession(t)
	sub := s.addSub(nil, true)
	<-sub.out
	s.ptmx.Close()
	s.viewport(sub, 25, 10, true)
	if s.cols != 80 || s.rows != 24 || s.revision != 0 || len(sub.out) != 0 {
		t.Fatal("failed resize published a new size")
	}
}

func TestGeometryQueueFailureRecomputes(t *testing.T) {
	s := geometrySession(t)
	a, b := s.addSub(nil, true), s.addSub(nil, true)
	s.viewport(a, 120, 30, true)
	s.viewport(b, 25, 10, true)
	for len(b.out) < cap(b.out) {
		b.out <- outbound{websocket.BinaryMessage, []byte("queued")}
	}
	s.publishOutput([]byte("live"))
	assertGeometry(t, s, 120, 30, 3)
	if s.subCount() != 1 {
		t.Fatal("slow subscriber not detached")
	}
	select {
	case <-b.stop:
	default:
		t.Fatal("writer not stopped")
	}
	// Detachment by the writer's deferred cleanup must remain idempotent.
	s.removeSub(b)
	assertGeometry(t, s, 120, 30, 3)
}

func TestGeometryWriterFailureRecomputes(t *testing.T) {
	s := geometrySession(t)
	a := s.addSub(nil, true)
	s.viewport(a, 120, 30, true)
	attached := make(chan *subscriber, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		attached <- s.addSub(conn, true)
	}))
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var b *subscriber
	select {
	case b = <-attached:
	case <-time.After(5 * time.Second):
		t.Fatal("attach timed out")
	}
	s.viewport(b, 25, 10, true)
	b.conn.Close()
	// No reader exists here: only the writer can detect and prune this failure.
	s.writeLoop(b)
	assertGeometry(t, s, 120, 30, 3)
	if s.subCount() != 1 {
		t.Fatal("failed writer remains attached")
	}
}

func TestGeometryConcurrentProposalsAndOutput(t *testing.T) {
	s := geometrySession(t)
	a, b := s.addSub(nil, true), s.addSub(nil, true)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			s.viewport(a, 100+i, 30, true)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			s.viewport(b, 25+i, 10, true)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			s.publishOutput([]byte("output"))
		}
	}()
	wg.Wait()
	s.mu.Lock()
	revision := s.revision
	s.mu.Unlock()
	assertGeometry(t, s, 34, 10, revision)
	for _, sub := range []*subscriber{a, b} {
		var last uint64
		first := true
		for len(sub.out) > 0 {
			msg := <-sub.out
			if msg.kind == websocket.TextMessage {
				size := decodeSize(t, msg)
				if !first && size.Revision != last+1 {
					t.Fatalf("nonconsecutive revision: %+v after %d", size, last)
				}
				first, last = false, size.Revision
			}
		}
		if last != revision {
			t.Fatalf("last revision %d, want %d", last, revision)
		}
	}
}

func TestAttachReplayAndLiveAreAtomic(t *testing.T) {
	for i := 0; i < 100; i++ {
		s := newTestSession()
		s.publishOutput([]byte("before"))
		var sub *subscriber
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); sub = s.addSub(nil, true) }()
		go func() { defer wg.Done(); s.publishOutput([]byte("!")) }()
		wg.Wait()
		gotSize := decodeSize(t, <-sub.out)
		if gotSize.Cols != 80 || gotSize.Rows != 24 {
			t.Fatalf("initial size: %+v", gotSize)
		}
		s.publishOutput([]byte("after"))
		var got []byte
		for len(sub.out) > 0 {
			msg := <-sub.out
			if msg.kind != websocket.BinaryMessage {
				t.Fatal("unexpected control")
			}
			got = append(got, msg.data...)
		}
		if string(got) != "before!after" {
			t.Fatalf("duplicated/reordered replay: %q", got)
		}
	}
}

type terminalWorkspace struct{ root string }

func (w terminalWorkspace) Current() string     { return w.root }
func (terminalWorkspace) OnChange(func(string)) {}

func terminalHarness(t *testing.T) (*Handler, string) {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("ENV", "")
	t.Setenv("PS1", "")
	h := New(terminalWorkspace{t.TempDir()}, "")
	server := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	t.Cleanup(server.Close)
	t.Cleanup(h.Close)
	return h, "ws" + strings.TrimPrefix(server.URL, "http") + "?session=geometry"
}

func dialTerminal(t *testing.T, url string, v2 bool) *websocket.Conn {
	t.Helper()
	if v2 {
		url += "&protocol=2"
	}
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readTerminal(t *testing.T, conn *websocket.Conn) outbound {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	return outbound{kind, data}
}

func awaitSize(t *testing.T, conn *websocket.Conn, cols, rows int) sizeEvent {
	t.Helper()
	for {
		msg := readTerminal(t, conn)
		if msg.kind == websocket.TextMessage {
			size := decodeSize(t, msg)
			if size.Cols != cols || size.Rows != rows {
				t.Fatalf("size %+v, want %dx%d", size, cols, rows)
			}
			return size
		}
	}
}

func sendViewport(t *testing.T, conn *websocket.Conn, cols, rows int, visible bool) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"type": "viewport", "cols": cols, "rows": rows, "visible": visible}); err != nil {
		t.Fatal(err)
	}
}

// expectOutput also asserts there is no unconsumed size/control preceding output.
func expectOutput(t *testing.T, conn *websocket.Conn, want string) {
	t.Helper()
	var output []byte
	for !bytes.Contains(output, []byte(want)) {
		msg := readTerminal(t, conn)
		if msg.kind != websocket.BinaryMessage {
			t.Fatalf("unexpected control: %s", msg.data)
		}
		output = append(output, msg.data...)
	}
}

func sendInput(t *testing.T, conn *websocket.Conn, input string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(input)); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketPTYGeometry(t *testing.T) {
	_, url := terminalHarness(t)
	a := dialTerminal(t, url, true)
	initial := decodeSize(t, readTerminal(t, a)) // first frame, even if shell already produced output
	if initial.Cols != 80 || initial.Rows != 24 || initial.Revision != 0 {
		t.Fatalf("initial: %+v", initial)
	}
	sendViewport(t, a, 120, 30, true)
	awaitSize(t, a, 120, 30)
	b := dialTerminal(t, url, true)
	if size := decodeSize(t, readTerminal(t, b)); size.Cols != 120 || size.Rows != 30 {
		t.Fatalf("attach: %+v", size)
	}
	sendViewport(t, b, 25, 10, true)
	if size := awaitSize(t, a, 25, 10); size.Revision != 2 {
		t.Fatalf("revision: %+v", size)
	}
	awaitSize(t, b, 25, 10)
	sendInput(t, a, "stty -echo; stty size\n")
	expectOutput(t, a, "10 25\r\n")
	sendViewport(t, b, 0, 0, false)
	awaitSize(t, a, 120, 30)
	awaitSize(t, b, 120, 30) // hidden peers still receive size
	sendInput(t, a, "stty size\n")
	expectOutput(t, a, "30 120\r\n")
	// Invalid v2 text is consumed, never executed as shell input.
	a.WriteMessage(websocket.TextMessage, []byte("printf BAD-CONTROL\\n\n"))
	sendViewport(t, b, 1, 10, true)
	sendInput(t, a, "stty size\n")
	expectOutput(t, a, "30 120\r\n")
	sendViewport(t, b, 25, 10, true)
	awaitSize(t, a, 25, 10)
	awaitSize(t, b, 25, 10)
	b.Close()
	awaitSize(t, a, 120, 30)
	sendInput(t, a, "stty size\n")
	expectOutput(t, a, "30 120\r\n")
}

func TestWebSocketLegacyNoSizeControls(t *testing.T) {
	_, url := terminalHarness(t)
	a := dialTerminal(t, url, true)
	decodeSize(t, readTerminal(t, a))
	sendViewport(t, a, 120, 30, true)
	awaitSize(t, a, 120, 30)
	legacy := dialTerminal(t, url, false)
	if err := legacy.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":25,"rows":10}`)); err != nil {
		t.Fatal(err)
	}
	awaitSize(t, a, 25, 10)
	if err := legacy.WriteMessage(websocket.TextMessage, []byte("stty -echo; stty size\n")); err != nil {
		t.Fatal(err)
	}
	expectOutput(t, legacy, "10 25\r\n") // every frame so far must be binary
	legacy.Close()
	awaitSize(t, a, 120, 30)
}

func TestWebSocketSizePrecedesSIGWINCHOutput(t *testing.T) {
	_, url := terminalHarness(t)
	conn := dialTerminal(t, url, true)
	decodeSize(t, readTerminal(t, conn))
	// Disable echo before installing the trap so command text cannot satisfy
	// the output assertions. Wake the shell after resize in case it defers traps.
	sendInput(t, conn, `stty -echo; trap 'printf "\nWINCH\n"; stty size' WINCH; printf '\nREADY\n'`+"\n")
	expectOutput(t, conn, "\r\nREADY\r\n")
	sendViewport(t, conn, 93, 27, true)
	seenSize := false
	var output []byte
	// A command sent after the proposal also exercises the real PTY input path.
	sendInput(t, conn, ":\n")
	for !bytes.Contains(output, []byte("27 93\r\n")) {
		msg := readTerminal(t, conn)
		if msg.kind == websocket.TextMessage {
			size := decodeSize(t, msg)
			if size.Cols != 93 || size.Rows != 27 {
				t.Fatalf("size: %+v", size)
			}
			seenSize = true
		} else {
			output = append(output, msg.data...)
			if bytes.Contains(output, []byte("WINCH")) && !seenSize {
				t.Fatal("SIGWINCH output preceded size")
			}
		}
	}
	if !seenSize || !bytes.Contains(output, []byte("WINCH")) {
		t.Fatalf("missing size/trap: %q", output)
	}
}

func TestGeometrySizePrecedesOutput(t *testing.T) {
	s := geometrySession(t)
	sub := s.addSub(nil, true)
	<-sub.out
	for i := 0; i < 20; i++ {
		s.viewport(sub, 80+i, 24, true)
		s.publishOutput([]byte(fmt.Sprintf("output-%d", i)))
		if i > 0 {
			decodeSize(t, <-sub.out)
		}
		msg := <-sub.out
		if msg.kind != websocket.BinaryMessage || string(msg.data) != fmt.Sprintf("output-%d", i) {
			t.Fatalf("ordering: %+v", msg)
		}
	}
}
