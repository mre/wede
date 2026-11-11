import type { CollaborationStatus } from '../hooks/useYDoc'

const descriptions: Record<CollaborationStatus, string> = {
  off: 'Collaboration is off for this tab.',
  connecting: 'Connecting to the document server. Editing is paused.',
  syncing: 'Synchronizing the document before enabling editing.',
  live: 'Connected and synchronized with the document server. This is not a disk-save acknowledgement.',
  disconnected: 'Connection lost. Editing is paused; this document is retained for reconnect. Keep this file selected until reconnected.',
  error: 'Could not connect to the document server. Editing is blocked; no single-user fallback. Check your connection and access.',
}

export default function CollaborationStatusIndicator({ status, viewer = false }: { status: CollaborationStatus; viewer?: boolean }) {
  return <span role="status" className="px-2 text-[11px] text-text-secondary" title={viewer ? 'Read-only REST snapshot. Live document access is unavailable for viewers.' : descriptions[status]}>
    Collaboration: {status}{viewer ? ' (read-only snapshot)' : ''}
  </span>
}
