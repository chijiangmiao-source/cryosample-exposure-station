// Pure derivations over a batch object as returned by the API.
// Kept separate from components so the transition boundaries are unit-testable.

export function remainingSeconds(batch) {
  return batch.allowedSeconds - batch.accumulatedSeconds
}

/** 柜内且未报废时才允许取出。 */
export function canTakeout(batch) {
  return !!batch && batch.state === 'in' && batch.status === 'usable'
}

/** 只有柜外状态才允许归还。 */
export function canReturn(batch) {
  return !!batch && batch.state === 'out'
}

export function isUsable(batch) {
  return !!batch && batch.status === 'usable'
}

export function stateLabel(batch) {
  if (!batch) return ''
  return batch.state === 'in' ? '柜内' : '柜外'
}

export function statusLabel(batch) {
  if (!batch) return ''
  return batch.status === 'usable' ? '可用' : '已报废'
}

export function eventTypeLabel(type) {
  return type === 'takeout' ? '取出' : type === 'return' ? '归还' : type
}

/** 该事件是否已被撤销。 */
export function isEventRevoked(ev) {
  return !!ev && ev.revokedAt != null
}

/** 流水中最后一条未撤销事件（可撤销目标）；没有则为 null。 */
export function latestActiveEvent(events) {
  if (!Array.isArray(events)) return null
  for (let i = events.length - 1; i >= 0; i--) {
    if (!isEventRevoked(events[i])) return events[i]
  }
  return null
}

/** 只有当前最后一条未撤销事件允许撤销。 */
export function canRevokeEvent(ev, events) {
  const latest = latestActiveEvent(events)
  return !!ev && !isEventRevoked(ev) && !!latest && latest.id === ev.id
}
