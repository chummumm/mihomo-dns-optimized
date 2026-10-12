// The controller owns version comparison, sources and installation. The page
// only follows its task and reconciles a new process after a core restart.
export type UpdateKind = 'kernel' | 'ui' | 'geodata' | 'rule-provider'
export type UpdateItemStatus = 'pending' | 'checking' | 'updated' | 'up_to_date' | 'skipped' | 'error' | 'restarting'
export type AutoUpdateState = 'scheduled' | 'waiting' | 'checking' | 'retrying' | 'unscheduled'

export interface UpdateItem {
    id: string
    kind: UpdateKind
    name: string
    status: UpdateItemStatus
    current_version?: string
    latest_version?: string
    auto_update?: boolean
    update_interval?: number
    interval_unit?: 'hours' | 'seconds'
    next_check?: string
    next_check_in_ms?: number
    auto_update_state?: AutoUpdateState
    reason?: string
    error?: string
}

export interface UpdateSnapshot {
    instance_id: string
    task_id?: string
    request_id?: string
    state: 'idle' | 'running' | 'completed' | 'partial' | 'restarting'
    started_at?: string
    finished_at?: string
    items: UpdateItem[]
}

const STATES = new Set(['idle', 'running', 'completed', 'partial', 'restarting'])
const KINDS = new Set(['kernel', 'ui', 'geodata', 'rule-provider'])
const ITEM_STATES = new Set(['pending', 'checking', 'updated', 'up_to_date', 'skipped', 'error', 'restarting'])
const AUTO_STATES = new Set(['scheduled', 'waiting', 'checking', 'retrying', 'unscheduled'])

export function readUpdateSnapshot (value: unknown): UpdateSnapshot | undefined {
    if (typeof value !== 'object' || value === null) return undefined
    const data = value as Partial<UpdateSnapshot>
    if (typeof data.instance_id !== 'string' || !data.instance_id || !STATES.has(data.state ?? '') || !Array.isArray(data.items)) return undefined
    if (data.task_id !== undefined && typeof data.task_id !== 'string') return undefined
    if (data.request_id !== undefined && (typeof data.request_id !== 'string' || !/^[a-f0-9]{32}$/.test(data.request_id))) return undefined
    if (data.state !== 'idle' && !data.task_id) return undefined
    for (const key of ['started_at', 'finished_at'] as const) {
        const value = data[key]
        if (value !== undefined && (typeof value !== 'string' || !Number.isFinite(Date.parse(value)))) return undefined
    }
    const ids = new Set<string>()
    for (const item of data.items) {
        if (typeof item !== 'object' || item === null || typeof item.id !== 'string' || typeof item.name !== 'string' || !KINDS.has(item.kind) || !ITEM_STATES.has(item.status) || ids.has(item.id)) return undefined
        ids.add(item.id)
        for (const key of ['current_version', 'latest_version', 'reason', 'error'] as const) {
            if (item[key] !== undefined && typeof item[key] !== 'string') return undefined
        }
        if (item.auto_update !== undefined && typeof item.auto_update !== 'boolean') return undefined
        if (item.update_interval !== undefined && (!Number.isFinite(item.update_interval) || item.update_interval < 0)) return undefined
        if (item.interval_unit !== undefined && item.interval_unit !== 'hours' && item.interval_unit !== 'seconds') return undefined
        if (item.next_check !== undefined && (typeof item.next_check !== 'string' || !Number.isFinite(Date.parse(item.next_check)))) return undefined
        if (item.next_check_in_ms !== undefined && (!Number.isSafeInteger(item.next_check_in_ms) || item.next_check_in_ms < 0)) return undefined
        if (item.auto_update_state !== undefined && !AUTO_STATES.has(item.auto_update_state)) return undefined
    }
    return data as UpdateSnapshot
}

// The server measures the live timer with its monotonic clock. Anchor that
// remaining delay to the browser's request START, not response arrival: a slow
// response must not postpone the displayed schedule. This conservatively shows
// a time up to one request round trip early. Monotonic elapsed time also lets a
// later browser wall-clock correction change the displayed calendar time.
export function nextUpdateTime (item: UpdateItem, requestStartedAt: number | undefined, browserNow: number, monotonicNow: number): number | undefined {
    if (!item.auto_update || item.auto_update_state === 'unscheduled') return undefined
    if (item.auto_update_state === undefined && (item.reason === 'not_configured' || item.reason === 'local_provider' || item.reason === 'embedded')) return undefined
    if (item.next_check_in_ms !== undefined && requestStartedAt !== undefined) {
        const deadline = browserNow + item.next_check_in_ms - Math.max(0, monotonicNow - requestStartedAt)
        return Number.isFinite(new Date(deadline).getTime()) ? deadline : undefined
    }
    // Older controllers may only provide an absolute timestamp. Keep it in the
    // browser's time zone; never replace a missing schedule with now + interval.
    return item.next_check ? Date.parse(item.next_check) : undefined
}

export function formatUpdateTime (value: number): string {
    return new Intl.DateTimeFormat(undefined, {
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        timeZoneName: 'short',
    }).format(value)
}

// A restart keeps the preceding manual results on screen, but its scheduling
// policy belongs to the current process. Never reuse an old remaining delay
// against the new request's browser clock.
export function currentUpdateSchedules (items: UpdateItem[], current: UpdateItem[]): UpdateItem[] {
    const byID = new Map(current.map(item => [item.id, item]))
    const merged = items.map(item => {
        const active = byID.get(item.id)
        byID.delete(item.id)
        return {
            ...item,
            auto_update: active?.auto_update,
            update_interval: active?.update_interval,
            interval_unit: active?.interval_unit,
            next_check: active?.next_check,
            next_check_in_ms: active?.next_check_in_ms,
            auto_update_state: active?.auto_update_state,
        }
    })
    return [...merged, ...byID.values()]
}

export function updateTaskActive (snapshot: UpdateSnapshot | undefined) {
    return snapshot?.state === 'running' || snapshot?.state === 'restarting'
}

export type RestartResult = 'verified' | 'unconfirmed'

export function reconcileUpdateRestart (previous: UpdateSnapshot | undefined, current: UpdateSnapshot): RestartResult | undefined {
    if (!updateTaskActive(previous) || previous?.instance_id === current.instance_id) return undefined
    const requested = previous?.items.find(item => item.kind === 'kernel')
    const running = current.items.find(item => item.kind === 'kernel')
    // A different instance alone only confirms reconnection. Compare against
    // the exact target that the old core reported, never infer install success
    // from a disconnected request or an arbitrary version change.
    return requested?.latest_version && requested.latest_version === running?.current_version && requested.current_version !== requested.latest_version
        ? 'verified'
        : 'unconfirmed'
}
