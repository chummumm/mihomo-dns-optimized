import type { DashboardUpdateStatus } from '@lib/request'

export const DASHBOARD_UPDATE_INTERVAL = 6 * 60 * 60 * 1000

export function compareDashboardVersions (left: string, right: string) {
    const valid = /^\d{1,6}\.\d{1,6}\.\d{1,6}$/
    if (!valid.test(left) || !valid.test(right)) return null
    const a = left.split('.').map(Number)
    const b = right.split('.').map(Number)
    for (let i = 0; i < 3; i++) {
        if (a[i] !== b[i]) return a[i] > b[i] ? 1 : -1
    }
    return 0
}

// Older cores may return their HTML fallback at this endpoint. Version
// metadata is optional: any configured external-ui-url can provide updates.
export function readDashboardUpdateStatus (value: unknown): DashboardUpdateStatus | null {
    if (value === null || typeof value !== 'object') return null
    const item = value as Record<string, unknown>
    if (typeof item.supported !== 'boolean' ||
        typeof item.update_available !== 'boolean' ||
        typeof item.updating !== 'boolean' ||
        typeof item.current_version !== 'string' ||
        typeof item.current_commit !== 'string') return null
    for (const key of ['reason', 'latest_version', 'latest_commit', 'check_error']) {
        if (item[key] !== undefined && (typeof item[key] !== 'string' || (item[key] as string).length > 512)) return null
    }
    if (item.current_version.length > 64 || item.current_commit.length > 64) return null
    for (const key of ['current_digest', 'latest_digest']) {
        if (item[key] !== undefined && (typeof item[key] !== 'string' || !/^[a-f0-9]{64}$/.test(item[key] as string))) return null
    }
    return item as unknown as DashboardUpdateStatus
}

export function isCoreHostedDashboard (location: Pick<Location, 'origin' | 'pathname'>, controllerOrigin: string) {
    return location.origin === controllerOrigin && (location.pathname === '/ui' || location.pathname.startsWith('/ui/'))
}

export function dashboardControllerOrigin (protocol: string, hostname: string, port: string | number): string | null {
    if ((protocol !== 'http:' && protocol !== 'https:') || !hostname || !/^\d{0,5}$/.test(String(port))) return null
    const host = hostname.includes(':') && !hostname.startsWith('[') ? `[${hostname}]` : hostname
    try {
        const url = new URL(`${protocol}//${host}:${port}`)
        if (url.username || url.password || url.pathname !== '/' || url.search || url.hash) return null
        return url.origin
    } catch {
        return null
    }
}
