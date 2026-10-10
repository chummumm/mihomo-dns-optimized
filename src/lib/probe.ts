import type { Proxy } from './request'

export const DEFAULT_PROBE_URL = 'https://www.gstatic.com/generate_204'

export function normalizeProbeURL (value: unknown): string | undefined {
    if (value === undefined || value === '') return DEFAULT_PROBE_URL
    if (typeof value !== 'string') return undefined
    const input = value.trim()
    if (input === '') return DEFAULT_PROBE_URL
    if (input.length > 2048) return undefined
    try {
        const url = new URL(input)
        if ((url.protocol !== 'https:' && url.protocol !== 'http:') || url.username || url.password || url.hash) return undefined
        return url.href
    } catch {
        return undefined
    }
}

const BUILTIN_POLICIES = new Set(['DIRECT', 'REJECT', 'REJECT-DROP', 'PASS'])
const BUILTIN_TYPES = new Set(['Direct', 'Reject', 'RejectDrop', 'Pass'])

export function supportsProxyDelay (proxy: Pick<Proxy, 'name' | 'type'>): boolean {
    return !BUILTIN_POLICIES.has(proxy.name) && !BUILTIN_TYPES.has(proxy.type)
}
