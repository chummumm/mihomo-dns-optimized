import type { DashboardUpdateStatus } from '@lib/request'

function parseDashboardVersion (version: string) {
    if (version.length > 64) return null
    const match = /^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/.exec(version)
    if (match === null || match[0] !== version) return null
    const prerelease = match[4]?.split('.') ?? []
    if (prerelease.some(part => /^0\d+$/.test(part))) return null
    return { core: match.slice(1, 4), prerelease }
}

function compareNumericIdentifier (left: string, right: string) {
    if (left.length !== right.length) return left.length > right.length ? 1 : -1
    return left === right ? 0 : left > right ? 1 : -1
}

// Keep refresh notices aligned with the core's strict SemVer comparison.
// Strings avoid precision loss for version numbers larger than JS integers.
export function compareDashboardVersions (left: string, right: string) {
    const a = parseDashboardVersion(left)
    const b = parseDashboardVersion(right)
    if (a === null || b === null) return null
    for (let i = 0; i < 3; i++) {
        const result = compareNumericIdentifier(a.core[i], b.core[i])
        if (result !== 0) return result
    }
    if (a.prerelease.length === 0 || b.prerelease.length === 0) {
        return a.prerelease.length === b.prerelease.length ? 0 : a.prerelease.length === 0 ? 1 : -1
    }
    for (let i = 0; i < Math.min(a.prerelease.length, b.prerelease.length); i++) {
        const left = a.prerelease[i]
        const right = b.prerelease[i]
        if (left === right) continue
        const leftNumeric = /^\d+$/.test(left)
        const rightNumeric = /^\d+$/.test(right)
        if (leftNumeric && rightNumeric) return compareNumericIdentifier(left, right)
        if (leftNumeric !== rightNumeric) return leftNumeric ? -1 : 1
        return left > right ? 1 : -1
    }
    return a.prerelease.length === b.prerelease.length ? 0 : a.prerelease.length > b.prerelease.length ? 1 : -1
}

export function canInstallDashboardUpdate (status: DashboardUpdateStatus | null) {
    return status !== null && status.supported && !status.updating && !status.check_error &&
        (status.update_available || status.reason === 'version-unavailable')
}

// Older cores may return their HTML fallback at this endpoint. Version
// metadata is optional for status and explicit manual installation; the core
// only considers a parseable, strictly newer version an available update.
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
    if (item.auto_update !== undefined && typeof item.auto_update !== 'boolean') return null
    if (item.update_interval !== undefined && (typeof item.update_interval !== 'number' || !Number.isSafeInteger(item.update_interval) || item.update_interval < 1)) return null
    if (item.next_check !== undefined && (typeof item.next_check !== 'string' || item.next_check.length > 64 || !Number.isFinite(Date.parse(item.next_check)))) return null
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
