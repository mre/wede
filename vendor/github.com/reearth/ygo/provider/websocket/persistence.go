package websocket

import (
	"context"
	"log"
)

// retireRoomLocked removes a live room and prevents its name from being reused
// until final persistence completes. Caller holds s.rmu exclusively.
func (s *Server) retireRoomLocked(name string, r *room) {
	delete(s.rooms, name)
	if r.persistStop != nil {
		s.retiring[name] = r
		select {
		case <-r.persistStop:
		default:
			close(r.persistStop)
		}
	}
}

// startPersistenceWorker spawns the goroutine that drains r.persistCh and
// forwards each update to the PersistenceAdapter. It must be called with
// r.persistCh, r.persistStop, and r.persistDone already initialised.
//
// The worker exits when r.persistStop is closed, draining buffered updates and
// flushing the retiring document before returning. Shutdown closes peer sockets
// first; their disconnect handlers stop persistence only after the read loops
// finish, so accepted updates cannot arrive after the final flush.
//
// If s.persistence implements PersistenceAdapterContext, store calls are made
// via StoreUpdateContext with a ctx that is cancelled when shutdown or stop
// fires. Otherwise falls back to StoreUpdate (existing behaviour).
func (s *Server) startPersistenceWorker(r *room, name string) {
	go func() {
		defer func() {
			s.rmu.Lock()
			if s.retiring[name] == r {
				delete(s.retiring, name)
			}
			close(r.persistDone)
			s.rmu.Unlock()
		}()
		dirty := false
		if flusher, ok := s.persistence.(PersistenceFlusher); ok {
			defer func() {
				if !dirty {
					return
				}
				s.safeHook("FlushDoc", func() {
					if err := flusher.FlushDoc(name, r.doc); err != nil {
						s.log().Error("flush retiring document failed", "room", name, "err", err)
					}
				})
			}()
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Cancel the persistence-adapter ctx when shutdown or stop fires.
		go func() {
			select {
			case <-s.shutdownCh:
			case <-r.persistStop:
			}
			cancel()
		}()

		store := func(update []byte) {
			dirty = true
			defer func() {
				if rv := recover(); rv != nil {
					log.Printf("ygo/websocket: StoreUpdate panic for room %q: %v", name, rv)
				}
			}()
			var err error
			if pac, ok := s.persistence.(PersistenceAdapterContext); ok {
				err = pac.StoreUpdateContext(ctx, name, update)
			} else {
				err = s.persistence.StoreUpdate(name, update)
			}
			if err != nil {
				log.Printf("ygo/websocket: StoreUpdate for room %q: %v", name, err)
			}
		}
		for {
			select {
			case update := <-r.persistCh:
				store(update)
			case <-r.persistStop:
				// Drain buffered updates before exiting.
				for {
					select {
					case update := <-r.persistCh:
						store(update)
					default:
						return
					}
				}
			}
		}
	}()
}
