# Collaboration changes in this fork

Presence and shared terminals do not imply shared document editing. Upstream's
collaboration preference defaults to off. Build the web frontend with
`VITE_WEDE_REQUIRE_COLLAB=true` to require shared document editing for owners and
editors, including browsers that previously stored `collab: false`. This is a
build-time flag, not a runtime server setting.

The editor shows a separate collaboration status. Editing waits for the document
socket to connect and complete initial synchronization. After disconnect, editing
pauses and the active document stays in memory for reconnect. Keep the file open
until it reconnects; reloading or switching files is not durable offline storage.
“Live” means connected and synchronized, not that a disk write was acknowledged.

All participants must select the same workspace entry and file. Separate workspace
entries over the same directory have separate document servers. Viewer invitations
currently show read-only file snapshots; use editor invitations for live sessions.
An older browser bundle can still use REST saves: all participants must reload
after deploying this policy. This is a frontend policy, not a server-side ban on
REST file writes.

REST Save, Save All, and editor formatting are bypassed in collaborative mode to
avoid replacing a live shared document with stale content. External writes from
terminals, formatters, or Git are not merged into an already-open CRDT document.
Do not mix those writes with active shared editing. Save-triggered LSP checks also
lack a persistence acknowledgement; language-server connection status is separate.

## Vendored persistence lifecycle extension

The fork changes `vendor/github.com/reearth/ygo/provider/websocket/` from the
`github.com/reearth/ygo v1.29.0` version pinned in `go.mod`. These are tracked source
changes, not a patch applied during image builds. Regenerating vendor will remove
them unless the dependency gains equivalent behavior first.

The optional `PersistenceFlusher.FlushDoc(room, doc)` interface flushes the actual
retiring document after pending updates drain. Room-name reuse waits for this
barrier, and shutdown waits for active and retiring rooms. WEDE's disk adapter
implements the interface and serializes timer and final writes. This prevents
accepted edits from disappearing when the last peer disconnects before the
write-back debounce. It does not guarantee persistence when disk writes fail;
errors are logged, with no automatic storage retry.

The regression tests in
`backend/internal/collabdoc/persistence_lifecycle_test.go` use real WebSocket
clients and cover last-peer disconnect, immediate reopen, and shutdown ordering.
Run them whenever updating or regenerating the vendored dependency.
