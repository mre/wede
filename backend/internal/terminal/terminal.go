package terminal

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

type WorkspaceProvider interface {
	Current() string
	OnChange(func(string))
}

const socketWriteTimeout = 5 * time.Second

type outbound struct {
	kind int
	data []byte
}

// Subscriber geometry and queue publication are guarded by session.mu.
// A single writer owns each socket; slow viewers cannot block the session.
type subscriber struct {
	conn       *websocket.Conn
	v2         bool
	cols, rows int
	visible    bool
	out        chan outbound
	stop       chan struct{}
}

func (sub *subscriber) write(kind int, data []byte) error {
	if err := sub.conn.SetWriteDeadline(time.Now().Add(socketWriteTimeout)); err != nil {
		return err
	}
	return sub.conn.WriteMessage(kind, data)
}

func (s *session) writeLoop(sub *subscriber) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	defer s.removeSub(sub)
	for {
		select {
		case <-sub.stop:
			return
		case msg, ok := <-sub.out:
			if !ok {
				return
			}
			if err := sub.write(msg.kind, msg.data); err != nil {
				return
			}
		case <-ticker.C:
			if err := sub.write(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// session holds a persistent pty shared by all subscribers in a room. The pty
// survives websocket reconnects and is fanned out to every connected subscriber.
type session struct {
	id         string
	ptmx       *os.File
	cmd        *exec.Cmd
	mu         sync.Mutex               // guards subs, closed, geometry and event publication
	subs       map[*subscriber]struct{} // all connected viewers (shared terminal)
	pmu        sync.Mutex               // serializes input writes to the pty
	buf        *ringBuffer              // scrollback buffer for replay on (re)connect
	done       chan struct{}
	closed     bool
	cols, rows int
	revision   uint64
	drainTimer *time.Timer // guards the total EOF drain, not just each socket write
}

// addSub atomically queues initial size, replay, then admits live output.
// The raw replay is a byte tail, not a terminal-state snapshot.
func (s *session) addSub(conn *websocket.Conn, v2 bool) *subscriber {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	sub := &subscriber{conn: conn, v2: v2, out: make(chan outbound, 64), stop: make(chan struct{})}
	if v2 {
		sub.out <- outbound{websocket.TextMessage, s.sizeMessage()}
	}
	if replay := s.buf.Bytes(); len(replay) > 0 {
		sub.out <- outbound{websocket.BinaryMessage, replay}
	}
	s.subs[sub] = struct{}{}
	return sub
}

// removeSub detaches a subscriber (does not kill the pty — the session persists).
func (s *session) removeSub(sub *subscriber) {
	s.mu.Lock()
	if s.dropSub(sub) {
		s.recomputeSize()
	}
	s.mu.Unlock()
}

// dropSub requires s.mu. Closing the socket also interrupts a blocked reader/writer.
func (s *session) dropSub(sub *subscriber) bool {
	if _, ok := s.subs[sub]; !ok {
		return false
	}
	delete(s.subs, sub)
	if len(s.subs) == 0 && s.drainTimer != nil {
		s.drainTimer.Stop()
	}
	close(sub.stop)
	if sub.conn != nil {
		sub.conn.Close()
	}
	return true
}

// subCount reports how many viewers are currently attached.
func (s *session) subCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// publishOutput makes buffering and fanout indivisible with respect to attachment
// and geometry changes. Callers transfer ownership of data.
func (s *session) publishOutput(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.buf.Write(data)
	for sub := range s.subs {
		s.enqueue(sub, outbound{websocket.BinaryMessage, data})
	}
	s.recomputeSize()
}

// enqueue requires s.mu; an overfull queue disconnects rather than dropping bytes.
func (s *session) enqueue(sub *subscriber, msg outbound) {
	select {
	case sub.out <- msg:
	default:
		s.dropSub(sub)
	}
}

func (s *session) sizeMessage() []byte {
	data, _ := json.Marshal(struct {
		Type     string `json:"type"`
		Cols     int    `json:"cols"`
		Rows     int    `json:"rows"`
		Revision uint64 `json:"revision"`
	}{"size", s.cols, s.rows, s.revision})
	return data
}

func validSize(cols, rows int) bool {
	return cols >= 2 && cols <= 1000 && rows >= 1 && rows <= 500
}

func (s *session) viewport(sub *subscriber, cols, rows int, visible bool) {
	if visible && !validSize(cols, rows) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sub]; !ok || s.closed {
		return
	}
	sub.cols, sub.rows, sub.visible = cols, rows, visible
	s.recomputeSize()
}

// recomputeSize requires s.mu. Setsize and notification publication share this
// lock with output publication, so SIGWINCH-generated output follows its size.
func (s *session) recomputeSize() {
	for !s.closed {
		cols, rows := 1001, 501
		for sub := range s.subs {
			if sub.visible && validSize(sub.cols, sub.rows) {
				cols = min(cols, sub.cols)
				rows = min(rows, sub.rows)
			}
		}
		if cols == 1001 || (cols == s.cols && rows == s.rows) {
			return
		}
		if err := pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
			log.Printf("[terminal] resize failed: %v", err)
			return
		}
		s.cols, s.rows = cols, rows
		s.revision++
		data := s.sizeMessage()
		before := len(s.subs)
		for sub := range s.subs {
			if sub.v2 {
				s.enqueue(sub, outbound{websocket.TextMessage, data})
			}
		}
		if len(s.subs) == before {
			return
		}
		// A failed notification may have removed the smallest viewer.
	}
}

// finishOutput seals the queues on PTY EOF/EIO. Writers consume all queued
// frames before closing their sockets. The shared deadline bounds the entire
// drain, including a write already in flight; per-write deadlines alone would
// allow a slow reader to stretch shutdown across every queued message.
func (s *session) finishOutput(timeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return // forced shutdown already closed the sockets
	}
	s.closed = true
	for sub := range s.subs {
		close(sub.out)
	}
	if len(s.subs) > 0 {
		s.drainTimer = time.AfterFunc(timeout, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.closeConns()
		})
	}
}

// closeConns forcibly closes sockets without draining (workspace/handler shutdown
// or the drain deadline). Caller must hold s.mu.
func (s *session) closeConns() {
	for sub := range s.subs {
		s.dropSub(sub)
	}
}

// ringBuffer stores the last N bytes of terminal output for replay on reconnect.
type ringBuffer struct {
	mu   sync.Mutex
	data []byte
	max  int
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{data: make([]byte, 0, size), max: size}
}

func (rb *ringBuffer) Write(p []byte) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.data = append(rb.data, p...)
	if len(rb.data) > rb.max {
		rb.data = rb.data[len(rb.data)-rb.max:]
	}
}

func (rb *ringBuffer) Bytes() []byte {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	out := make([]byte, len(rb.data))
	copy(out, rb.data)
	return out
}

// parseOrigins splits a space-separated frame_ancestors value into a set of
// allowed origin strings.  Each entry should be an origin like
// "https://vulos.org" (no trailing slash, no path).
func parseOrigins(frameAncestors string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, o := range strings.Fields(frameAncestors) {
		set[o] = struct{}{}
	}
	return set
}

// checkOrigin enforces WebSocket origin validation.
//
//   - No Origin header (e.g. a native tool or curl) → allowed (non-browser client).
//   - Origin matches the Host header → same-origin → allowed.
//   - Origin is in the allowedOrigins set (from frame_ancestors config) → allowed
//     so that the Vulos OS shell, which legitimately embeds wede in an iframe,
//     can also open the terminal WebSocket.
//   - Anything else → rejected.
func checkOrigin(r *http.Request, allowedOrigins map[string]struct{}) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin header: non-browser client (e.g. curl, native app). Allow.
		return true
	}

	// Derive the expected same-origin value from the Host header.
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// X-Forwarded-Proto from a trusted reverse proxy takes precedence.
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" || proto == "http" {
		scheme = proto
	}
	selfOrigin := scheme + "://" + r.Host
	if origin == selfOrigin {
		return true
	}

	// Allowed cross-origin embedding (e.g. Vulos shell).
	if _, ok := allowedOrigins[origin]; ok {
		return true
	}

	log.Printf("[terminal] rejected WebSocket upgrade from origin %q (host=%s)", origin, r.Host)
	return false
}

type Handler struct {
	ws       WorkspaceProvider
	mu       sync.Mutex
	sessions map[string]*session
	upgrader websocket.Upgrader
}

// New creates a terminal handler.  allowedOrigins is the space-separated list
// from the frame_ancestors config (e.g. "https://vulos.org").  When empty,
// only same-origin WebSocket upgrades are allowed.
func New(ws WorkspaceProvider, allowedOrigins string) *Handler {
	allowed := parseOrigins(allowedOrigins)
	h := &Handler{
		ws:       ws,
		sessions: make(map[string]*session),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return checkOrigin(r, allowed)
			},
		},
	}
	// Kill all terminal sessions when workspace changes so new ones open in the new directory
	ws.OnChange(func(string) {
		h.mu.Lock()
		for id, s := range h.sessions {
			s.mu.Lock()
			s.closeConns()
			s.ptmx.Close()
			s.cmd.Process.Kill()
			s.closed = true
			s.mu.Unlock()
			delete(h.sessions, id)
		}
		h.mu.Unlock()
	})
	return h
}

// Close terminates every terminal session owned by this handler — closing the
// active websocket, the PTY, and killing the shell process. Called when the
// owning room is closed.
func (h *Handler) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, s := range h.sessions {
		s.mu.Lock()
		s.closeConns()
		if s.ptmx != nil {
			s.ptmx.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			s.cmd.Process.Kill() //nolint:errcheck
		}
		s.closed = true
		s.mu.Unlock()
		delete(h.sessions, id)
	}
}

func (h *Handler) getOrCreateSession(id string) (*session, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if s, ok := h.sessions[id]; ok {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if !closed {
			log.Printf("[terminal] reattaching to existing session %q", id)
			return s, true, nil
		}
	}
	log.Printf("[terminal] creating new session %q (existing sessions: %d)", id, len(h.sessions))

	shell := os.Getenv("SHELL")
	if shell == "" {
		if runtime.GOOS == "windows" {
			shell = "cmd.exe"
		} else {
			shell = "/bin/bash"
		}
	}

	dir := h.ws.Current()
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = home
	}

	cmd := exec.Command(shell, "-l")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		return nil, false, err
	}

	s := &session{
		id:   id,
		ptmx: ptmx,
		cmd:  cmd,
		subs: make(map[*subscriber]struct{}),
		buf:  newRingBuffer(64 * 1024), // 64KB scrollback
		done: make(chan struct{}),
		cols: 80,
		rows: 24,
	}
	h.sessions[id] = s

	// pty reader goroutine — reads from the pty and fans output out to every
	// connected subscriber (shared terminal), buffering for late-joiner replay.
	go func() {
		buf := make([]byte, 32768)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				s.publishOutput(data)
			}
			if err != nil {
				s.finishOutput(socketWriteTimeout)
				close(s.done)
				h.mu.Lock()
				if h.sessions[id] == s {
					delete(h.sessions, id)
				}
				h.mu.Unlock()
				return
			}
		}
	}()

	return s, false, nil
}

// ListSessions returns active session IDs via HTTP.
func (h *Handler) ListSessions(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	ids := make([]string, 0, len(h.sessions))
	for id, s := range h.sessions {
		s.mu.Lock()
		if !s.closed {
			ids = append(ids, id)
		}
		s.mu.Unlock()
	}
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"sessions": ids})
}

// handleControl consumes all v2 text, and only recognized resize controls for
// legacy clients. Invalid controls never become shell input.
func (s *session) handleControl(sub *subscriber, msg []byte) bool {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(msg, &header); err != nil {
		return sub.v2
	}
	if (sub.v2 && header.Type != "viewport") || (!sub.v2 && header.Type != "resize") {
		return sub.v2
	}
	var control struct {
		Cols    int   `json:"cols"`
		Rows    int   `json:"rows"`
		Visible *bool `json:"visible"`
	}
	if json.Unmarshal(msg, &control) == nil {
		if !sub.v2 {
			s.viewport(sub, control.Cols, control.Rows, true)
		} else if control.Visible != nil {
			s.viewport(sub, control.Cols, control.Rows, *control.Visible)
		}
	}
	return true
}

func (h *Handler) HandleWS(w http.ResponseWriter, r *http.Request) {
	// The auth token is conveyed via the "auth.<token>" WebSocket subprotocol so
	// it never appears in server access logs.  The Middleware already validated
	// the request before this handler is reached; we echo the chosen subprotocol
	// back to the client so the browser's WebSocket handshake succeeds.
	var chosenProto string
	for _, p := range websocket.Subprotocols(r) {
		if strings.HasPrefix(p, "auth.") {
			chosenProto = p
			break
		}
	}
	var upgradeHeader http.Header
	if chosenProto != "" {
		upgradeHeader = http.Header{"Sec-Websocket-Protocol": {chosenProto}}
	}

	conn, err := h.upgrader.Upgrade(w, r, upgradeHeader)
	if err != nil {
		log.Println("websocket upgrade error:", err)
		return
	}

	// Session ID from query param only (token no longer passed in URL).
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		// Fallback: derive session from the token subprotocol value so existing
		// single-tab sessions continue to work even without an explicit session param.
		if chosenProto != "" {
			sessionID = strings.TrimPrefix(chosenProto, "auth.")
		}
	}

	log.Printf("[terminal] ws connect: session=%q", sessionID)
	v2 := r.URL.Query().Get("protocol") == "2"
	s, _, err := h.getOrCreateSession(sessionID)
	if err != nil {
		log.Println("pty start error:", err)
		kind := websocket.TextMessage
		if v2 {
			kind = websocket.BinaryMessage
		}
		(&subscriber{conn: conn}).write(kind, []byte("Error starting terminal: "+err.Error()))
		conn.Close()
		return
	}

	sub := s.addSub(conn, v2)
	if sub == nil {
		conn.Close()
		return
	}
	defer s.removeSub(sub)
	go s.writeLoop(sub)

	// Per-connection ping/pong keepalive.
	conn.SetReadDeadline(time.Now().Add(60 * time.Second)) //nolint:errcheck
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second)) //nolint:errcheck
		return nil
	})

	// Any subscriber may type; PTY input writes remain serialized via s.pmu.
	// V2 reserves text frames for controls and binary frames for terminal input.
	log.Printf("[terminal] session %q: subscriber attached (viewers=%d)", sessionID, s.subCount())
	for {
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if msgType == websocket.TextMessage && s.handleControl(sub, msg) {
			continue
		}
		s.pmu.Lock()
		_, werr := io.WriteString(s.ptmx, string(msg))
		s.pmu.Unlock()
		if werr != nil {
			return
		}
	}
}
