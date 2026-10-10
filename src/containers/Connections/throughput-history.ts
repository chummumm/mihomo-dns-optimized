export const RATE_WINDOW_MS = 60_000
export const RATE_STALE_MS = 10_000
export const RATE_SAMPLE_LIMIT = 64

export interface RateReading {
    up: number
    down: number
    at: number
}

export interface RateSample extends RateReading {
    span: number
    segment: number
}

interface Totals {
    uploadTotal: number
    downloadTotal: number
}

export function pruneRateSamples (samples: RateSample[], now: number) {
    let expired = 0
    while (expired < samples.length && now - samples[expired].at > RATE_WINDOW_MS) expired++
    if (expired > 0) samples.splice(0, expired)
}

export function rateSampleX (at: number, now: number, width: number) {
    return width * (1 - (now - at) / RATE_WINDOW_MS)
}

// One time bucket per second, weighted by its measured duration. Even if a
// controller sends snapshots faster than once a second, one minute stays bounded.
export class ThroughputHistory {
    readonly samples: RateSample[] = []
    private previous: (Totals & { at: number }) | undefined
    private segment = 0

    feed (totals: Totals, now: number): RateReading {
        pruneRateSamples(this.samples, now)
        const previous = this.previous
        const valid = Number.isFinite(now) && Number.isFinite(totals.uploadTotal) && Number.isFinite(totals.downloadTotal) && totals.uploadTotal >= 0 && totals.downloadTotal >= 0
        this.previous = valid ? { ...totals, at: now } : undefined
        const elapsed = previous === undefined ? 0 : now - previous.at
        if (!valid || previous === undefined || elapsed <= 0 || elapsed > RATE_STALE_MS || totals.uploadTotal < previous.uploadTotal || totals.downloadTotal < previous.downloadTotal) {
            this.segment++
            return { up: 0, down: 0, at: now }
        }

        const up = (totals.uploadTotal - previous.uploadTotal) * 1000 / elapsed
        const down = (totals.downloadTotal - previous.downloadTotal) * 1000 / elapsed
        const sample = { up, down, at: now, span: elapsed, segment: this.segment }
        const last = this.samples[this.samples.length - 1]
        if (last !== undefined && last.segment === sample.segment && Math.floor(last.at / 1000) === Math.floor(now / 1000)) {
            const span = last.span + elapsed
            sample.up = (last.up * last.span + up * elapsed) / span
            sample.down = (last.down * last.span + down * elapsed) / span
            sample.span = span
            this.samples[this.samples.length - 1] = sample
        } else {
            this.samples.push(sample)
            if (this.samples.length > RATE_SAMPLE_LIMIT) this.samples.shift()
        }
        return { up, down, at: now }
    }
}
