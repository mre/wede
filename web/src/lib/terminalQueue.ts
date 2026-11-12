// xterm.write completes asynchronously. Keep control operations behind earlier
// output, including the reset that separates two WebSocket replay streams.
export function terminalQueue(onOverflow: () => void) {
  let pending: { run: (done: () => void) => void; bytes: number }[] = []
  let bytes = 0
  let busy = false
  let draining = false
  let disposed = false

  function drain() {
    if (draining || disposed) return
    draining = true
    while (!busy && pending.length && !disposed) {
      const item = pending.shift()!
      busy = true
      item.run(() => {
        bytes -= item.bytes
        busy = false
        drain()
      })
    }
    draining = false
  }

  return {
    push(run: (done: () => void) => void, size = 0) {
      if (disposed) return
      if (bytes + size > 4 * 1024 * 1024 || pending.length >= 4096) {
        onOverflow()
        return
      }
      bytes += size
      pending.push({ run, bytes: size })
      drain()
    },
    clear() {
      // An in-flight write cannot be cancelled; retain its barrier and accounting.
      bytes -= pending.reduce((sum, item) => sum + item.bytes, 0)
      pending = []
    },
    dispose() {
      disposed = true
      pending = []
    },
  }
}
