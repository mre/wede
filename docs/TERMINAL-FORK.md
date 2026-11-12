# Shared terminal sizing

A shared PTY has one logical grid. Every connected browser must render the output
at that same size, even when their panels have different pixel dimensions.

This fork negotiates terminal protocol 2 with `?protocol=2`. Browsers send visible
viewport capacity, not resize commands. The server chooses the minimum columns
and rows among visible subscribers and broadcasts the authoritative size. Hidden
panes and background tabs withdraw from sizing. With no visible subscribers, the
last size is retained; new sessions start at 80 columns by 24 rows.

A narrow visible participant can therefore leave unused space in a wider panel.
That is intentional. Hiding their pane or disconnecting releases the constraint.
Each browser uses the bundled JetBrains Mono Variable font and remeasures after
font loading or changes to its layout.

Protocol 2 uses binary frames for input/output and text JSON for viewport/size
controls. Size events and output share an ordered stream. Attachment sends the
current size, then replay, then live output. Socket writes and queues are bounded;
normal shell exit drains final output before closing, subject to a deadline.

Older clients remain supported but cannot display shared geometry accurately;
all participants should reload after deployment. Previously malformed terminal
history is not repaired. Replay remains a bounded raw byte tail, not a complete
terminal-state snapshot, and may include history from different grid sizes. Use
a fresh terminal when checking the fix.
