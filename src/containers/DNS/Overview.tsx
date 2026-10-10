import { useCallback, useEffect } from 'react'

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

function DNSTrend ({ series }: { series: DNSBucket[] }) {
    const { t } = useDNSI18n()
    const max = Math.max(1, ...series.map(bucket => bucket.queries))
    const first = series[0]
    const last = series[series.length - 1]
    return <Card className="dns-section">
        <div className="dns-section-heading"><h2>{t('trend')}</h2><span className="dns-muted">{t('perMinute')}</span></div>
        {series.length === 0
            ? <p className="dns-empty">{t('empty')}</p>
            : <>
                <svg className="dns-trend" viewBox="0 0 960 195" role="img" aria-label={t('trend')}>
                    <title>{t('trend')}</title>
                    {[0, 0.5, 1].map(ratio => <g key={ratio}>
                        <line x1="40" x2="940" y1={170 - ratio * 145} y2={170 - ratio * 145} className="dns-grid-line" />
                        <text x="32" y={174 - ratio * 145} textAnchor="end">{formatDNSNumber(max * ratio)}</text>
                    </g>)}
                    <polyline points={trendPoints(series, 'queries', max)} className="dns-line dns-line-queries" />
                    <polyline points={trendPoints(series, 'cache_hits', max)} className="dns-line dns-line-cache" />
                    <polyline points={trendPoints(series, 'errors', max)} className="dns-line dns-line-errors" />
                    <text x="40" y="190">{formatDNSTime(first.time)}</text>
                    <text x="940" y="190" textAnchor="end">{formatDNSTime(last.time)}</text>
                </svg>
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
    return <Card className="dns-section">
        <h2>{title}</h2>
        <p className="dns-muted dns-caption">{t('retainedScope')}</p>
        {items.length === 0
            ? <p className="dns-empty">{t('empty')}</p>
            : <ol className="dns-top-list">
                {items.slice(0, 20).map(item => <li key={item.name}>
                    <span className="dns-top-name" title={item.name}>{item.name === 'unknown' ? t('unknown') : item.name}</span>
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
        { label: t('cacheRatio'), value: cacheHitRatio(counts.cache_fresh + counts.cache_stale, counts.queries), hint: `${t('fresh')}: ${formatDNSNumber(counts.cache_fresh)} · ${t('stale')}: ${formatDNSNumber(counts.cache_stale)} · ${t('processCacheRatio')}: ${cacheHitRatio(data.totals.cache_fresh + data.totals.cache_stale, data.totals.queries)}` },
        { label: t('errorsLabel'), value: formatDNSNumber(counts.errors), hint: `${t('rejected')}: ${formatDNSNumber(counts.reject)} · ${t('dropped')}: ${formatDNSNumber(counts.drop)}` },
        { label: t('averageLatency'), value: `${formatDNSNumber(counts.elapsed_ms_avg, 2)} ms`, hint: `${t('approximateP95')}: ${formatDNSNumber(counts.elapsed_ms_p95, 2)} ms` },
    ]

    return <>
        <div className="dns-section-heading"><h2>{t('last24h')}</h2><DNSStatus {...resource} /></div>
        <div className="dns-metrics">
            {cards.map(card => <Card key={card.label} className="dns-metric">
                <span className="dns-muted">{card.label}</span>
                <strong>{card.value}</strong>
                <span className="dns-muted dns-caption">{card.hint}</span>
            </Card>)}
        </div>
        <p className="dns-muted dns-caption">{t('cacheDefinition')}</p>
        <DNSTrend series={data.series} />
        <div className="dns-top-grid">
            <TopList title={t('topDomains')} items={data.top_domains} />
            <TopList title={t('topClients')} items={data.top_clients} />
        </div>
    </>
}
