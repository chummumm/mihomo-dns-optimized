import { useCallback, useEffect, useMemo, useState, type PointerEvent } from 'react'

import { Card } from '@components'
import type { Client } from '@lib/request'

import { DNSStatus } from './Status'
import { cacheHitRatio, completedMinuteQPS, formatDNSNumber, formatDNSTime, trendPoints } from './helpers'
import { useDNSI18n, useDNSResource } from './hooks'
import type { DNSBucket, DNSTopEntry } from './types'

interface OverviewProps {
    client: Client
    instanceID: string
    instanceChanged: () => void
}

// Chart frame, matching the coordinates trendPoints() draws into.
const LEFT = 40
const RIGHT = 940
const BASE = 170
const HEIGHT = 145

function DNSTrend ({ series }: { series: DNSBucket[] }) {
    const { t } = useDNSI18n()
    const [hover, setHover] = useState<number>()
    const max = Math.max(1, ...series.map(bucket => bucket.queries))
    const first = series[0]
    const last = series[series.length - 1]
    const queries = useMemo(() => trendPoints(series, 'queries', max), [series, max])
    const area = queries ? `${LEFT},${BASE} ${queries} ${RIGHT},${BASE}` : ''
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
                        {[0, 0.5, 1].map(ratio => <g key={ratio}>
                            <line x1={LEFT} x2={RIGHT} y1={BASE - ratio * HEIGHT} y2={BASE - ratio * HEIGHT} className="dns-grid-line" />
                            <text x="32" y={174 - ratio * HEIGHT} textAnchor="end">{formatDNSNumber(max * ratio)}</text>
                        </g>)}
                        <polygon points={area} className="dns-area-queries" />
                        <polyline points={queries} className="dns-line dns-line-queries" />
                        <polyline points={trendPoints(series, 'cache_hits', max)} className="dns-line dns-line-cache" />
                        <polyline points={trendPoints(series, 'errors', max)} className="dns-line dns-line-errors" />
                        <text x={LEFT} y="190">{formatDNSTime(first.time)}</text>
                        <text x={RIGHT} y="190" textAnchor="end">{formatDNSTime(last.time)}</text>
                        {active && <g className="dns-cursor" aria-hidden="true">
                            <line x1={activeX} x2={activeX} y1={BASE - HEIGHT} y2={BASE} />
                            <circle cx={activeX} cy={BASE - active.queries / max * HEIGHT} r="4" className="dns-cursor-queries" />
                            <circle cx={activeX} cy={BASE - active.cache_hits / max * HEIGHT} r="3.5" className="dns-cursor-cache" />
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
