import type { DNSBucket } from './types'

// The overview chart is a plain time-series line chart. Each range is drawn at
// a resolution that keeps it readable: per-minute points for the last hour,
// short averages for longer windows. Values stay "per minute" in every range,
// so the y-axis keeps the same meaning when the reader switches ranges.
export type TrendRange = '1' | '6' | '24'

export const TREND_RANGES: Array<{ value: TrendRange, hours: number, bucket: number }> = [
    { value: '1', hours: 1, bucket: 1 },
    { value: '6', hours: 6, bucket: 5 },
    { value: '24', hours: 24, bucket: 10 },
]

const MINUTE = 60000

export interface TrendPoint {
    // first minute of the bucket and the end of its last minute (exclusive)
    start: number
    end: number
    // midpoint of the minutes actually present; the x position of the point
    time: number
    minutes: number
    // per-minute averages over the bucket
    queries: number
    cache: number
    errors: number
    // totals over the bucket
    totalQueries: number
}

function localMinute (time: number) {
    // getTimezoneOffset() is UTC − local in minutes, so this is minutes since the local epoch.
    return Math.floor(time / MINUTE) - new Date(time).getTimezoneOffset()
}

/**
 * Turns the kernel's minute buckets into chart points for one range.
 *
 * The final API bucket is the kernel's current, still-filling minute; it is left
 * out so the line never shows a false drop at its right edge. Buckets are aligned
 * to the local clock (e.g. :00, :10, :20), so ticks and readouts read naturally.
 */
export function buildTrend (series: DNSBucket[], hours: number, bucketMinutes: number): TrendPoint[] {
    const minutes: Array<{ time: number, bucket: DNSBucket }> = []
    for (const bucket of series) {
        const time = Date.parse(bucket.time)
        if (Number.isFinite(time)) minutes.push({ time, bucket })
    }
    if (minutes.length > 1) minutes.pop()
    if (minutes.length === 0) return []

    const last = minutes[minutes.length - 1].time
    const from = last - (hours * 60 - 1) * MINUTE
    const size = Math.max(1, Math.round(bucketMinutes))
    const points: TrendPoint[] = []
    let current: { key: number, items: Array<{ time: number, bucket: DNSBucket }> } | undefined

    function flush () {
        if (current === undefined || current.items.length === 0) return
        const { items } = current
        let queries = 0
        let cache = 0
        let errors = 0
        let time = 0
        for (const item of items) {
            queries += item.bucket.queries
            cache += item.bucket.cache_hits
            errors += item.bucket.errors
            time += item.time + MINUTE / 2
        }
        const n = items.length
        points.push({
            start: items[0].time,
            end: items[n - 1].time + MINUTE,
            time: time / n,
            minutes: n,
            queries: queries / n,
            cache: cache / n,
            errors: errors / n,
            totalQueries: queries,
        })
    }

    for (const item of minutes) {
        if (item.time < from) continue
        const key = Math.floor(localMinute(item.time) / size)
        if (current === undefined || current.key !== key) {
            flush()
            current = { key, items: [] }
        }
        current.items.push(item)
    }
    flush()
    return points
}

function niceStep (raw: number) {
    if (!(raw > 0)) return 1
    const power = Math.pow(10, Math.floor(Math.log10(raw)))
    const fraction = raw / power
    const nice = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 2.5 ? 2.5 : fraction <= 5 ? 5 : 10
    return nice * power
}

/** Round y-axis ticks from zero, e.g. 0 / 25 / 50 / 75 / 100. */
export function niceTicks (max: number, count = 4) {
    if (!(max > 0)) return [0, 1]
    const step = niceStep(max / count)
    const top = Math.ceil(max / step - 1e-9) * step
    const ticks: number[] = []
    for (let value = 0; value <= top + step / 2; value += step) ticks.push(Number(value.toPrecision(12)))
    return ticks
}

const TICK_STEPS = [5, 10, 15, 30, 60, 120, 180, 240, 360, 720]

/** Clock-aligned x-axis ticks, spaced so labels never crowd each other. */
export function timeTicks (from: number, to: number, width: number, minGap = 64) {
    const span = to - from
    if (!(span > 0) || !(width > 0)) return []
    const step = TICK_STEPS.find(minutes => width / (span / (minutes * MINUTE)) >= minGap) ?? TICK_STEPS[TICK_STEPS.length - 1]
    const ticks: number[] = []
    // Start at the first local clock multiple of the step inside the window.
    const offset = new Date(from).getTimezoneOffset()
    let local = Math.ceil((Math.ceil(from / MINUTE) - offset) / step) * step
    for (;;) {
        const time = (local + offset) * MINUTE
        if (time > to) break
        ticks.push(time)
        local += step
    }
    return ticks
}

export function formatClock (time: number) {
    const date = new Date(time)
    return `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`
}

type Point = [number, number]

// Monotone cubic interpolation (Fritsch–Carlson): smooth, but never overshoots
// the data, so a smoothed line cannot dip below zero or invent a peak.
export function monotonePath (points: Point[]) {
    const n = points.length
    if (n === 0) return ''
    const head = `M${points[0][0].toFixed(1)},${points[0][1].toFixed(1)}`
    if (n === 1) return `${head} h0.1`
    const dx: number[] = []
    const slope: number[] = []
    for (let i = 0; i < n - 1; i++) {
        dx.push(points[i + 1][0] - points[i][0])
        slope.push(dx[i] === 0 ? 0 : (points[i + 1][1] - points[i][1]) / dx[i])
    }
    const tangent: number[] = [slope[0]]
    for (let i = 1; i < n - 1; i++) {
        if (slope[i - 1] * slope[i] <= 0) {
            tangent.push(0)
        } else {
            const w1 = 2 * dx[i] + dx[i - 1]
            const w2 = dx[i] + 2 * dx[i - 1]
            tangent.push((w1 + w2) / (w1 / slope[i - 1] + w2 / slope[i]))
        }
    }
    tangent.push(slope[n - 2])
    let d = head
    for (let i = 0; i < n - 1; i++) {
        const [x0, y0] = points[i]
        const [x1, y1] = points[i + 1]
        const h = dx[i] / 3
        d += ` C${(x0 + h).toFixed(1)},${(y0 + tangent[i] * h).toFixed(1)} ${(x1 - h).toFixed(1)},${(y1 - tangent[i + 1] * h).toFixed(1)} ${x1.toFixed(1)},${y1.toFixed(1)}`
    }
    return d
}
