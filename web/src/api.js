// Thin fetch wrapper around the timing-station API.
// In dev, Vite proxies /api to the Go server; in the Docker image nginx does.

const BASE = '/api'

async function request(path, opts = {}) {
  const res = await fetch(BASE + path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts
  })
  const text = await res.text()
  const data = text ? JSON.parse(text) : null
  if (!res.ok) {
    const err = new Error(data?.error?.message || `HTTP ${res.status}`)
    err.status = res.status
    err.code = data?.error?.code || 'unknown'
    throw err
  }
  return data
}

export const api = {
  health: () => request('/health'),
  createBatch: (body) =>
    request('/batches', { method: 'POST', body: JSON.stringify(body) }),
  getBatch: (barcode, asOf) => {
    // asOf is an optional RFC3339 whole-second "Z" timestamp: when present the
    // server adds a non-persistent risk projection (?asOf=...); omitted, the
    // response keeps its settled-values-only semantics.
    const qs = asOf ? `?asOf=${encodeURIComponent(asOf)}` : ''
    return request(`/batches/${encodeURIComponent(barcode)}${qs}`)
  },
  listEvents: (barcode) => request(`/batches/${encodeURIComponent(barcode)}/events`),
  postEvent: (barcode, body) =>
    request(`/batches/${encodeURIComponent(barcode)}/events`, {
      method: 'POST',
      body: JSON.stringify(body)
    }),
  revokeEvent: (barcode, id, body) =>
    request(`/batches/${encodeURIComponent(barcode)}/events/${id}/revoke`, {
      method: 'POST',
      body: JSON.stringify(body)
    }),
  // Cabinet slot occupancy (independent of the exposure state machine):
  // PUT places/moves an in-cabinet usable batch into a scanned slot; DELETE
  // vacates the batch's slot. A takeout releases the slot automatically.
  putLocation: (barcode, location) =>
    request(`/batches/${encodeURIComponent(barcode)}/location`, {
      method: 'PUT',
      body: JSON.stringify({ location })
    }),
  clearLocation: (barcode) =>
    request(`/batches/${encodeURIComponent(barcode)}/location`, {
      method: 'DELETE'
    })
}
