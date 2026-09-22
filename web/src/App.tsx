import { useEffect, useState } from 'react'

type ProbeState = 'unknown' | 'up' | 'down'

/**
 * I02 scope: skeleton page proving the toolchain (TS strict + Vite build)
 * and the dev-proxy wiring to the control-plane API probes. Domain screens
 * arrive with I16/I17.
 */
export function App() {
  const [api, setApi] = useState<ProbeState>('unknown')

  useEffect(() => {
    let cancelled = false
    const check = async () => {
      try {
        const res = await fetch('/api/healthz')
        if (!cancelled) setApi(res.ok ? 'up' : 'down')
      } catch {
        if (!cancelled) setApi('down')
      }
    }
    void check()
    const timer = setInterval(check, 5000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [])

  return (
    <main style={{ fontFamily: 'system-ui, sans-serif', padding: '2rem' }}>
      <h1>SAOAF Control Plane</h1>
      <p>
        API probe:{' '}
        <strong style={{ color: api === 'up' ? 'green' : api === 'down' ? 'red' : 'gray' }}>
          {api}
        </strong>
      </p>
      <p>
        Control-plane management screens (Resource Hub, Sovereignty Operations)
        land with issues I16/I17.
      </p>
    </main>
  )
}
