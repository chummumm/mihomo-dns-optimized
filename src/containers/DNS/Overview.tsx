import { useCallback, useEffect, useId, useMemo, useState, type PointerEvent } from 'react'

import { Card } from '@components'
import type { Client } from '@lib/request'

import { DNSStatus } from './Status'
import { cacheHitRatio, completedMinuteQPS, formatDNSNumber, formatDNSTime } from './helpers'
import { useDNSI18n, useDNSResource } from './hooks'
import type { DNSBucket, DNSTopEntry } from './types'

interface OverviewProps {
    client: Client
    instanceID: string
    instanceChanged: () => void
}

// Chart frame in viewBox units (the same frame trendPoints() in helpers.ts uses).
const LEFT = 40
const RIGHT = 940
const BASE = 170
const HEIGHT = 145
// The 24-hour series is drawn through at most this many bins: each bin's mean
// makes the line, and its lowest and highest minute make a faint band, so a
// one-minute spike still shows while the line itself stays readable.
const BINS = 192

type Point = [number, number]

// Monotone cubic interpolation (Fritsch–Carlson): smooth, but never overshoots
// the data, so a smoothed line cannot dip below zero or invent a peak.
function monotonePath (points: Point[], move = true) {
    const n = points.length
    if (n === 0) return ''
    const head = `${move ? 'M' : 'L'}${points[0][0].toFixed(1)},${points[0][1].toFixed(1)}`
    if (n === 1) return head
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

interface Binned { mean: Point[], low: Point[], high: Point[] }

function binSeries (series: DNSBucket[], field: 'queries' | 'cache_hits' | 'errors', max: number): Binned {
    const total = series.length
    const denominator = Math.max(total - 1, 1)
    const count = Math.min(total, BINS)
    const x = (index: number) => LEFT + index / denominator * (RIGHT - LEFT)
    const y = (value: number) => BASE - value / Math.max(max, 1) * HEIGHT
    const result: Binned = { mean: [], low: [], high: [] }
    for (let b = 0; b < count; b++) {
        const from = Math.floor(b * total / count)
        const to = Math.max(from + 1, Math.floor((b + 1) * total / count))
        let sum = 0
        let low = Infinity
        let high = -Infinity
        for (let i = from; i < to; i++) {
            const v = series[i][field]
            sum += v
            if (v < low) low = v
            if (v > high) high = v
        }
        const center = x((from + to - 1) / 2)
        result.mean.push([center, y(sum / (to - from))])
        result.low.push([center, y(low)])
        result.high.push([center, y(high)])
    }
    // Pin the ends to the frame so the line spans the whole window.
    for (const line of [result.mean, result.low, result.high]) {
        if (line.length > 0) {
            line[0] = [LEFT, line[0][1]]
            line[line.length - 1] = [RIGHT, line[line.length - 1][1]]
        }
    }
    return result
}

const morph = (d: string) => ({ d: `path("${d}")` } as React.CSSProperties)

function DNSTrend ({ series }: { series: DNSBucket[] }) {
    const { t } = useDNSI18n()
    const id = useId().replace(/:/g, '')
    const [hover, setHover] = useState<number>()
    const max = Math.max(1, ...series.map(bucket => bucket.queries))
    const first = series[0]
    const last = series[series.length - 1]
    const paths = useMemo(() => {
        const queries = binSeries(series, 'queries', max)
        const cache = binSeries(series, 'cache_hits', max)
        const errors = binSeries(series, 'errors', max)
        const queriesLine = monotonePath(queries.mean)
        return {
            queries: queriesLine,
            queriesArea: queriesLine ? `${queriesLine} L${RIGHT},${BASE} L${LEFT},${BASE} Z` : '',
            band: queries.high.length ? `${monotonePath(queries.high)} ${monotonePath(queries.low.slice().reverse(), false)} Z` : '',
            cache: monotonePath(cache.mean),
            errors: monotonePath(errors.mean),
        }
    }, [series, max])
    const denominator = Math.max(series.length - 1, 1)
    const active = hover === undefined ? undefined : series[hover]
    const activeX = hover === undefined ? 0 : LEFT + hover / denominator * (RIGHT - LEFT)

    function track (event: PointerEvent<SVGRectElement>) {
        const rect = event.currentTarget.getBoundingClientRect()
        const ratio = Math.min(1, Math.max(0, (event.clientX - rect.left) / rect.width))
        setHover(Math.round(ratio * denominator))
    }

    return <Card className="dns-section dns-trend-card">
        <div className="dns-section-heading"><h2>{t('trend')}</h2><span className="dns-muted">{t('perMinute')}</span></div>
        {series.length === 0
            ? <p className="dns-empty">{t('empty')}</p>
            : <>
                <div className="dns-trend-frame">
                    <svg className="dns-trend" viewBox="0 0 960 195" role="img" aria-label={t('trend')}>
                        <title>{t('trend')}</title>
                        <defs>
                            <linearGradient id={`dns-fill-${id}`} x1="0" y1="0" x2="0" y2="1">
                                <stop offset="0" className="dns-fill-top" />
                                <stop offset="1" className="dns-fill-bottom" />
                            </linearGradient>
                        </defs>
                        {[0, 0.5, 1].map(ratio => <g key={ratio}>
                            <line x1={LEFT} x2={RIGHT} y1={BASE - ratio * HEIGHT} y2={BASE - ratio * HEIGHT} className="dns-grid-line" />
                            <text x="32" y={174 - ratio * HEIGHT} textAnchor="end">{formatDNSNumber(max * ratio)}</text>
                        </g>)}
                        <path d={paths.band} style={morph(paths.band)} className="dns-band-queries" />
                        <path d={paths.queriesArea} style={morph(paths.queriesArea)} className="dns-area-queries" fill={`url(#dns-fill-${id})`} />
                        <path d={paths.cache} style={morph(paths.cache)} pathLength={1} className="dns-line dns-line-cache" />
                        <path d={paths.queries} style={morph(paths.queries)} pathLength={1} className="dns-line dns-line-queries" />
                        <path d={paths.errors} style={morph(paths.errors)} pathLength={1} className="dns-line dns-line-errors" />
                        <text x={LEFT} y="190">{formatDNSTime(first.time)}</text>
                        <text x={RIGHT} y="190" textAnchor="end">{formatDNSTime(last.time)}</text>
                        {active && <g className="dns-cursor" aria-hidden="true" style={{ transform: `translateX(${activeX}px)` }}>
                            <line x1={0} x2={0} y1={BASE - HEIGHT} y2={BASE} />
                            <circle cx={0} cy={BASE - active.queries / max * HEIGHT} r="4" className="dns-cursor-queries" />
                            <circle cx={0} cy={BASE - active.cache_hits / max * HEIGHT} r="3.5" className="dns-cursor-cache" />
                        </g>}
                        <rect x={LEFT} y={BASE - HEIGHT} width={RIGHT - LEFT} height={HEIGHT} className="dns-hit-area" onPointerMove={track} onPointerDown={track} onPointerLeave={() => setHover(undefined)} />
                    </svg>
                    {active && <div className={activeX > 700 ? 'dns-readout is-flipped' : 'dns-readout'} style={{ left: `${(activeX / 960) * 100}%` }} aria-hidden="true">
                        <span className="dns-readout-time">{formatDNSTime(active.time)}</span>
                        <span className="dns-legend-queries">{t('queries')} <b>{formatDNSNumber(active.queries)}</b></span>
                        <span className="dns-legend-cache">{t('cacheHits')} <b>{formatDNSNumber(active.cache_hits)}</b></span>
                        <span className="dns-legend-errors">{t('errorsLabel')} <b>{formatDNSNumber(active.errors)}</b></span>
                    </div>}
                </div>
                <div className="dns-chart-legend">
                    <span className="dns-legend-queries">{t('queries')}</span>
                    <span className="dns-legend-cache">{t('cacheHits')}</span>
                    <span className="dns-legend-errors">{t('errorsLabel')}</span>
                </div>
            </>}
    </Card>
}

function TopList ({ title, items }: { title: string, items: DNSTopEntry[] }) {
    const { t } = useDNSI18n()
    const shown = items.slice(0, 20)
    const max = Math.max(1, ...shown.map(item => item.count))
    return <Card className="dns-section dns-top-card">
        <h2>{title}</h2>
        <p className="dns-muted dns-caption">{t('retainedScope')}</p>
        {items.length === 0
            ? <p className="dns-empty">{t('empty')}</p>
            : <ol className="dns-top-list">
                {shown.map((item, index) => <li key={item.name}>
                    <span className="dns-top-rank">{index + 1}</span>
                    <span className="dns-top-name" title={item.name}>{item.name === 'unknown' ? t('unknown') : item.name}</span>
                    <span className="dns-top-bar" aria-hidden="true"><span style={{ width: `${item.count / max * 100}%` }} /></span>
                    <strong>{formatDNSNumber(item.count)}</strong>
                </li>)}
            </ol>}
    </Card>
}

export function DNSOverview ({ client, instanceID, instanceChanged }: OverviewProps) {
    const { t } = useDNSI18n()
    const fetcher = useCallback(async (signal: AbortSignal) => (await client.getDNSStats(signal)).data, [client])
    const resource = useDNSResource(fetcher, 5000)
    const data = resource.data?.instance_id === instanceID ? resource.data : undefined

    useEffect(() => {
        if (resource.data && resource.data.instance_id !== instanceID) instanceChanged()
    }, [resource.data, instanceID, instanceChanged])

    if (!data) return <DNSStatus {...resource} />
    const counts = data.last_24h
    const cards = [
        { label: t('queries'), value: formatDNSNumber(counts.queries), hint: `${t('processTotal')}: ${formatDNSNumber(data.totals.queries)}` },
        { label: 'QPS', value: formatDNSNumber(completedMinuteQPS(data.series), 2), hint: t('qpsScope') },
        { label: t('cacheRatio'), value: cacheHitRatio(counts.cache_fresh + counts.cache_stale, counts.queries), hint: `${t('fresh')}: ${formatDNSNumber(counts.cache_fresh)} · ${t('stale')}: ${formatDNSNumber(counts.cache_stale)} · ${t('processCacheRatio')}: ${cacheHitRatio(data.totals.cache_fresh + data.totals.cache_stale, data.totals.queries)}`, cache: true },
        { label: t('errorsLabel'), value: formatDNSNumber(counts.errors), hint: `${t('rejected')}: ${formatDNSNumber(counts.reject)} · ${t('dropped')}: ${formatDNSNumber(counts.drop)}` },
        { label: t('averageLatency'), value: `${formatDNSNumber(counts.elapsed_ms_avg, 2)} ms`, hint: `${t('approximateP95')}: ${formatDNSNumber(counts.elapsed_ms_p95, 2)} ms` },
    ]
    const share = (value: number) => counts.queries > 0 ? `${value / counts.queries * 100}%` : '0%'

    return <>
        <div className="dns-section-heading"><h2>{t('last24h')}</h2><DNSStatus {...resource} /></div>
        <Card className="dns-metrics">
            {cards.map(card => <div key={card.label} className="dns-metric">
                <span className="dns-metric-label">{card.label}</span>
                <strong>{card.value}</strong>
                {card.cache && <span className="dns-cache-split" aria-hidden="true">
                    <span className="is-fresh" style={{ width: share(counts.cache_fresh) }} />
                    <span className="is-stale" style={{ width: share(counts.cache_stale) }} />
                </span>}
                <span className="dns-muted dns-caption">{card.hint}</span>
            </div>)}
        </Card>
        <p className="dns-muted dns-caption dns-definition">{t('cacheDefinition')}</p>
        <DNSTrend series={data.series} />
        <div className="dns-top-grid">
            <TopList title={t('topDomains')} items={data.top_domains} />
            <TopList title={t('topClients')} items={data.top_clients} />
        </div>
    </>
}
