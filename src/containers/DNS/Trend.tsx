import classnames from 'classnames'
import { useId, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type PointerEvent } from 'react'

import { ButtonSelect, Card } from '@components'

import { formatDNSNumber } from './helpers'
import { useDNSI18n } from './hooks'
import { TREND_RANGES, buildTrend, formatClock, monotonePath, niceTicks, timeTicks, type TrendPoint, type TrendRange } from './trend'
import type { DNSBucket } from './types'

// The chart is drawn in real pixels (the viewBox follows the measured width),
// so axis text stays the same size on a phone and on a wide monitor.
const HEIGHT = 228
const PAD = { top: 14, right: 16, bottom: 30, left: 46 }

type Series = 'queries' | 'cache' | 'errors'
const SERIES: Series[] = ['cache', 'errors', 'queries']

const morph = (d: string) => ({ d: `path("${d}")` } as React.CSSProperties)

function perMinute (value: number) {
    return formatDNSNumber(value, value > 0 && value < 10 ? 1 : 0)
}

function useWidth (element: React.RefObject<HTMLElement>) {
    const [width, setWidth] = useState(0)
    useLayoutEffect(() => {
        const current = element.current
        if (current === null) return
        const measure = () => setWidth(Math.max(0, Math.floor(current.clientWidth)))
        measure()
        if (typeof ResizeObserver !== 'function') return
        const observer = new ResizeObserver(measure)
        observer.observe(current)
        return () => observer.disconnect()
    }, [element])
    return width
}

function TrendPlot ({ points, range }: { points: TrendPoint[], range: TrendRange }) {
    const { t } = useDNSI18n()
    const frame = useRef<HTMLDivElement>(null)
    const width = useWidth(frame)
    const gradient = `dns-trend-fill-${useId().replace(/:/g, '')}`
    const [hover, setHover] = useState<number>()

    const plotWidth = Math.max(1, width - PAD.left - PAD.right)
    const plotHeight = HEIGHT - PAD.top - PAD.bottom
    const base = PAD.top + plotHeight
    const first = points[0].time
    const last = points[points.length - 1].time
    const span = Math.max(1, last - first)
    const x = (time: number) => points.length === 1 ? PAD.left + plotWidth / 2 : PAD.left + (time - first) / span * plotWidth

    const ticks = useMemo(() => niceTicks(Math.max(...points.map(point => Math.max(point.queries, point.cache, point.errors)))), [points])
    const top = ticks[ticks.length - 1]
    const y = (value: number) => base - value / top * plotHeight

    const paths = useMemo(() => {
        const line = (key: Series) => monotonePath(points.map(point => [x(point.time), y(point[key])]))
        const queries = line('queries')
        return {
            queries,
            cache: line('cache'),
            errors: line('errors'),
            area: `${queries} L${x(last).toFixed(1)},${base.toFixed(1)} L${x(first).toFixed(1)},${base.toFixed(1)} Z`,
        }
        // x and y are derived from the values listed here.
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [points, width, top])

    const xTicks = useMemo(() => timeTicks(first, last, plotWidth), [first, last, plotWidth])
    const active = hover === undefined ? undefined : points[Math.min(hover, points.length - 1)]
    const activeX = active === undefined ? 0 : x(active.time)

    function nearest (clientX: number, rect: DOMRect) {
        const time = first + (clientX - rect.left - PAD.left) / plotWidth * span
        let low = 0
        let high = points.length - 1
        while (low < high) {
            const middle = (low + high) >> 1
            if (points[middle].time < time) low = middle + 1
            else high = middle
        }
        return low > 0 && Math.abs(points[low - 1].time - time) <= Math.abs(points[low].time - time) ? low - 1 : low
    }

    function track (event: PointerEvent<SVGSVGElement>) {
        setHover(nearest(event.clientX, event.currentTarget.getBoundingClientRect()))
    }

    function keydown (event: KeyboardEvent<SVGSVGElement>) {
        const current = hover ?? points.length - 1
        const next = event.key === 'ArrowLeft'
            ? Math.max(0, current - 1)
            : event.key === 'ArrowRight'
                ? Math.min(points.length - 1, current + 1)
                : event.key === 'Home' ? 0 : event.key === 'End' ? points.length - 1 : undefined
        if (event.key === 'Escape') { setHover(undefined); return }
        if (next === undefined) return
        event.preventDefault()
        setHover(next)
    }

    const timeLabel = (point: TrendPoint) => point.minutes <= 1 && point.end - point.start <= 60000
        ? formatClock(point.start)
        : `${formatClock(point.start)} – ${formatClock(point.end)}`

    return <div ref={frame} className="dns-trend-frame">
        {width > 0 && <svg
            className="dns-trend"
            width={width}
            height={HEIGHT}
            viewBox={`0 0 ${width} ${HEIGHT}`}
            role="img"
            tabIndex={0}
            aria-label={`${t('trend')} · ${t(`trendRange.h${range}`)}`}
            onPointerMove={track}
            onPointerDown={track}
            onPointerLeave={() => setHover(undefined)}
            onKeyDown={keydown}
            onFocus={() => setHover(value => value ?? points.length - 1)}
            onBlur={() => setHover(undefined)}>
            <defs>
                <linearGradient id={gradient} x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0" className="dns-fill-top" />
                    <stop offset="1" className="dns-fill-bottom" />
                </linearGradient>
            </defs>
            <g className="dns-axis" aria-hidden="true">
                {ticks.map(value => <g key={value}>
                    <line x1={PAD.left} x2={PAD.left + plotWidth} y1={y(value)} y2={y(value)} className={value === 0 ? 'dns-grid-line is-base' : 'dns-grid-line'} />
                    <text x={PAD.left - 10} y={y(value)} dy="0.32em" textAnchor="end">{formatDNSNumber(value, value > 0 && value < 10 ? 1 : 0)}</text>
                </g>)}
                {xTicks.map(time => {
                    const position = x(time)
                    const anchor = position - PAD.left < 22 ? 'start' : PAD.left + plotWidth - position < 22 ? 'end' : 'middle'
                    return <g key={time}>
                        <line x1={position} x2={position} y1={base} y2={base + 4} className="dns-tick-line" />
                        <text x={position} y={base + 18} textAnchor={anchor}>{formatClock(time)}</text>
                    </g>
                })}
            </g>
            <g key={range} className="dns-lines">
                <path d={paths.area} style={morph(paths.area)} className="dns-area-queries" fill={`url(#${gradient})`} />
                {SERIES.map(key => <path key={key} d={paths[key]} style={morph(paths[key])} pathLength={1} className={`dns-line dns-line-${key}`} />)}
            </g>
            {active !== undefined && <g className="dns-cursor" aria-hidden="true" style={{ transform: `translateX(${activeX}px)` }}>
                <line x1={0} x2={0} y1={PAD.top} y2={base} />
                {SERIES.map(key => <circle key={key} cx={0} cy={y(active[key])} r="4" className={`dns-cursor-${key}`} />)}
            </g>}
        </svg>}
        {active !== undefined && <div className={classnames('dns-readout', { 'is-flipped': activeX > width * 0.62 })} style={{ left: `${activeX}px`, top: `${PAD.top}px` }} aria-hidden="true">
            <span className="dns-readout-time">{timeLabel(active)}</span>
            <span className="dns-legend-queries">{t('queries')} <b>{perMinute(active.queries)}</b></span>
            <span className="dns-legend-cache">{t('cacheHits')} <b>{perMinute(active.cache)}</b></span>
            <span className="dns-legend-errors">{t('errorsLabel')} <b>{perMinute(active.errors)}</b></span>
            {active.minutes > 1 && <span className="dns-readout-total">{t('bucketTotal')} <b>{formatDNSNumber(active.totalQueries)}</b></span>}
        </div>}
    </div>
}

export function DNSTrend ({ series }: { series: DNSBucket[] }) {
    const { t } = useDNSI18n()
    const [range, setRange] = useState<TrendRange>('24')
    const config = TREND_RANGES.find(item => item.value === range) ?? TREND_RANGES[TREND_RANGES.length - 1]
    const points = useMemo(() => buildTrend(series, config.hours, config.bucket), [series, config.hours, config.bucket])
    const empty = points.length === 0 || points.every(point => point.queries === 0 && point.cache === 0 && point.errors === 0)
    const unit = config.bucket > 1 ? `${t('perMinute')} · ${config.bucket} ${t('minuteAverage')}` : t('perMinute')

    return <Card className="dns-section dns-trend-card">
        <div className="dns-section-heading dns-trend-heading">
            <div className="dns-trend-title">
                <h2>{t('trend')}</h2>
                <span className="dns-muted dns-trend-unit">{unit}</span>
            </div>
            <ButtonSelect
                className="dns-range-select"
                label={t('trendRangeLabel')}
                options={TREND_RANGES.map(item => ({ label: t(`trendRange.h${item.value}`), value: item.value }))}
                value={range}
                onSelect={value => setRange(value as TrendRange)} />
        </div>
        {points.length === 0
            ? <p className="dns-empty">{t('empty')}</p>
            : <>
                <TrendPlot points={points} range={range} />
                <div className="dns-chart-legend">
                    <span className="dns-legend-queries">{t('queries')}</span>
                    <span className="dns-legend-cache">{t('cacheHits')}</span>
                    <span className="dns-legend-errors">{t('errorsLabel')}</span>
                    {empty && <span className="dns-chart-empty-note">{t('emptyRange')}</span>}
                </div>
            </>}
    </Card>
}
