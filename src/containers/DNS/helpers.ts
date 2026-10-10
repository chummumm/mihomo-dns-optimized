import type { DNSBucket } from './types'

export function formatDNSNumber (value: number, digits = 0) {
    return new Intl.NumberFormat(undefined, { maximumFractionDigits: digits }).format(value)
}

export function formatDNSTime (value: string | number, full = false) {
    const date = new Date(value)
    if (!Number.isFinite(date.getTime())) return '—'
    return full ? date.toLocaleString() : date.toLocaleTimeString()
}

export function cacheHitRatio (hits: number, queries: number) {
    return queries > 0 ? `${formatDNSNumber(hits * 100 / queries, 1)}%` : '—'
}

export function completedMinuteQPS (series: DNSBucket[]) {
    // The final API bucket is the kernel's current minute. Use the previous
    // complete bucket so browser clock differences cannot change the rate.
    return series.length >= 2 ? series[series.length - 2].queries / 60 : 0
}

// The API returns at most one day of minute buckets, never individual log rows.
export function trendPoints (series: DNSBucket[], field: 'queries' | 'cache_hits' | 'errors', max: number) {
    const denominator = Math.max(series.length - 1, 1)
    return series.map((bucket, index) => `${40 + index / denominator * 900},${170 - bucket[field] / Math.max(max, 1) * 145}`).join(' ')
}
