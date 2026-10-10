import { isIP } from '../../lib/ip'
import type { Connections } from '../../lib/request'

export const ALL_CONNECTIONS = 'all'
export const INTERNAL_CONNECTIONS = 'internal'
export const UNKNOWN_CONNECTIONS = 'unknown'

function clientIP (value: unknown): string | undefined {
    if (typeof value !== 'string' || !isIP(value)) return undefined
    if (!value.includes(':')) return value === '0.0.0.0' ? undefined : value

    // URL uses the browser's IPv6 parser to canonicalize equivalent addresses.
    // Preserve a link-local interface zone, which is not part of the address.
    const [address, zone] = value.split('%')
    try {
        const normalized = new URL(`http://[${address}]/`).hostname.slice(1, -1)
        if (normalized === '::' || normalized === '::ffff:0:0') return undefined
        return zone ? `${normalized}%${zone}` : normalized
    } catch {
        return undefined
    }
}

export function getConnectionSourceKey (connection: Partial<Connections>): string {
    // Real resolver sockets can retain a client's inbound metadata. Only the
    // explicit kernel flag distinguishes them from that client's logical query.
    if (connection.dns === true) return INTERNAL_CONNECTIONS
    const source = clientIP(connection.metadata?.sourceIP)
    if (source !== undefined) return `device:${source}`
    return connection.metadata?.type === 'Inner' ? INTERNAL_CONNECTIONS : UNKNOWN_CONNECTIONS
}
