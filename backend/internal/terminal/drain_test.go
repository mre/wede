package terminal

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Deliberately do not start the writer: tests can force PTY termination while
// every output frame is still queued, independent of scheduler/network timing.
func drainSubscriber(t *testing.T, s *session, v2 bool) (*subscriber, *websocket.Conn) {
	t.Helper()
	attached := make(chan *subscriber, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		attached <- s.addSub(conn, v2)
	}))
	t.Cleanup(server.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	select {
	case sub := <-attached:
		t.Cleanup(func() { s.removeSub(sub) })
		return sub, client
	case <-time.After(time.Second):
		t.Fatal("attach timed out")
		return nil, nil
	}
}

func readUntilClosed(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var got []byte
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatalf("socket did not close: %v", err)
			}
			return got
		}
		if kind != websocket.BinaryMessage {
			t.Fatalf("unexpected frame: %d %q", kind, data)
		}
		got = append(got, data...)
	}
}

func TestNormalTerminationDrainsQueuedOutput(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2=%t", v2), func(t *testing.T) {
			s := newTestSession()
			s.publishOutput([]byte("replay"))
			sub, client := drainSubscriber(t, s, v2)
			want := []byte("replay")
			// Fill the queue exactly: EOF must not require a spare sentinel slot.
			for len(sub.out) < cap(sub.out) {
				data := []byte(fmt.Sprintf("final-%02d\n", len(sub.out)))
				s.publishOutput(data)
				want = append(want, data...)
			}
			s.finishOutput(socketWriteTimeout)
			select {
			case <-sub.stop:
				t.Fatal("EOF discarded pending output")
			default:
			}
			if s.addSub(nil, v2) != nil {
				t.Fatal("ended session accepted subscriber")
			}
			// Late producers and controls must not send into sealed queues.
			s.publishOutput([]byte("late"))
			s.viewport(sub, 25, 10, true)
			go s.writeLoop(sub)
			if v2 {
				decodeSize(t, readTerminal(t, client))
			}
			if got := readUntilClosed(t, client); !bytes.Equal(got, want) {
				t.Fatalf("lost/reordered final output: got %q, want %q", got, want)
			}
			if s.subCount() != 0 {
				t.Fatal("drained subscriber was not removed")
			}
		})
	}
}

func TestNormalTerminationDrainIsBounded(t *testing.T) {
	s := newTestSession()
	sub, client := drainSubscriber(t, s, true)
	s.publishOutput([]byte("pending"))
	// A writer that never gets scheduled cannot extend the total drain deadline.
	s.finishOutput(20 * time.Millisecond)
	select {
	case <-sub.stop:
	case <-time.After(time.Second):
		t.Fatal("drain deadline did not force closure")
	}
	if got := readUntilClosed(t, client); len(got) != 0 {
		t.Fatalf("unexpected output: %q", got)
	}
	if s.subCount() != 0 {
		t.Fatal("timed-out subscriber was not removed")
	}
}

func TestForcedClosureDoesNotWaitForDrain(t *testing.T) {
	s := newTestSession()
	sub, client := drainSubscriber(t, s, true)
	s.publishOutput([]byte("pending"))
	s.finishOutput(time.Hour)
	s.mu.Lock()
	s.closeConns()
	s.mu.Unlock()
	select {
	case <-sub.stop:
	default:
		t.Fatal("forced closure waited for drain")
	}
	readUntilClosed(t, client)
}

func TestWebSocketPTYFinalOutputBeforeExit(t *testing.T) {
	_, url := terminalHarness(t)
	conn := dialTerminal(t, url, true)
	decodeSize(t, readTerminal(t, conn))
	sendInput(t, conn, "stty -echo; printf '\\nREADY\\n'\n")
	expectOutput(t, conn, "\r\nREADY\r\n")
	sendInput(t, conn, "printf 'final-output'; exit\n")
	if got := readUntilClosed(t, conn); !bytes.Contains(got, []byte("final-output")) {
		t.Fatalf("missing final PTY output: %q", got)
	}
}
