import { useState, useEffect, useRef } from 'react'

export function useSpeed() {
  const [speeds, setSpeeds] = useState({})
  const wsRef = useRef(null)

  useEffect(() => {
    let unmounted = false
    let reconnectTimer = null

    function connect() {
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
      const ws = new WebSocket(proto + '//' + location.host + '/api/v1/ws/speed')
      wsRef.current = ws

      ws.onmessage = (e) => {
        try {
          const data = JSON.parse(e.data)
          if (data.speeds) {
            const map = {}
            for (const s of data.speeds) map[s.node_id] = s
            setSpeeds(map)
          }
        } catch {}
      }

      ws.onclose = () => {
        if (!unmounted) {
          reconnectTimer = setTimeout(connect, 3000)
        }
      }

      ws.onerror = () => ws.close()
    }

    connect()

    return () => {
      unmounted = true
      clearTimeout(reconnectTimer)
      if (wsRef.current) wsRef.current.close()
    }
  }, [])

  return speeds
}

export function fmtSpeed(bps) {
  if (!bps || bps <= 0) return '0 B/s'
  if (bps < 1024) return bps.toFixed(0) + ' B/s'
  if (bps < 1048576) return (bps / 1024).toFixed(1) + ' KB/s'
  if (bps < 1073741824) return (bps / 1048576).toFixed(2) + ' MB/s'
  return (bps / 1073741824).toFixed(2) + ' GB/s'
}
