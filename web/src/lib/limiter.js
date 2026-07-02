// createLimiter returns a function that runs at most `max` async tasks
// concurrently, queueing the rest. Used to cap "test all" so N rules don't fire
// N probe-chain requests at once (each of which fans out to every hop on the
// server), which otherwise floods both the panel and the agents.
export function createLimiter(max) {
  let active = 0
  const queue = []
  const pump = () => {
    if (active >= max || queue.length === 0) return
    active++
    const { fn, resolve, reject } = queue.shift()
    Promise.resolve()
      .then(fn)
      .then(resolve, reject)
      .finally(() => { active--; pump() })
  }
  return (fn) => new Promise((resolve, reject) => {
    queue.push({ fn, resolve, reject })
    pump()
  })
}
