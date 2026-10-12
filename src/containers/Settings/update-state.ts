// The controller owns version comparison, sources and installation. The page
// only follows its task and reconciles a new process after a core restart.
export type UpdateKind = 'kernel' | 'ui' | 'geodata' | 'rule-provider'
export type UpdateItemStatus = 'pending' | 'checking' | 'updated' | 'up_to_date' | 'skipped' | 'error' | 'restarting'

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
    }
    return data as UpdateSnapshot
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
