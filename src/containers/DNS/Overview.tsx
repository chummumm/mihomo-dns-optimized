import { useCallback, useEffect } from 'react'

import { Card } from '@components'
import type { Client } from '@lib/request'

import { DNSStatus } from './Status'
import { DNSTrend } from './Trend'
import { cacheHitRatio, completedMinuteQPS, formatDNSNumber } from './helpers'
import { useDNSI18n, useDNSResource } from './hooks'
import type { DNSTopEntry } from './types'

interface OverviewProps {
    client: Client
    instanceID: string
    instanceChanged: () => void
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
