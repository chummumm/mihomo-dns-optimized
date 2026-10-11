import { type ColumnDef } from '@tanstack/react-table'
import { useCallback, useEffect, useMemo } from 'react'

import { Card } from '@components'
import type { Client } from '@lib/request'

import { DNSStatus } from './Status'
import { DNSTable } from './Table'
import { cacheHitRatio, formatDNSNumber } from './helpers'
import { useDNSI18n, useDNSResource } from './hooks'
import type { DNSUpstream } from './types'

interface UpstreamsProps {
    client: Client
    instanceID: string
    instanceChanged: () => void
}

const upstreamID = (upstream: DNSUpstream) => upstream.id

function elapsed (value: number | undefined, count: number) {
    return count > 0 && value !== undefined && Number.isFinite(value) && value >= 0
        ? `${formatDNSNumber(value, 2)} ms`
        : '—'
}

export function DNSUpstreamPage ({ client, instanceID, instanceChanged }: UpstreamsProps) {
    const { t } = useDNSI18n()
    const fetcher = useCallback(async (signal: AbortSignal) => (await client.getDNSUpstreams(signal)).data, [client])
    const resource = useDNSResource(fetcher, 5000)
    const data = resource.data?.instance_id === instanceID ? resource.data : undefined
    const legacyLatency = useMemo(() => data !== undefined && data.items.length > 0 && data.items.every(upstream => upstream.success_elapsed_ms_avg === undefined), [data])

    useEffect(() => {
        if (resource.data && resource.data.instance_id !== instanceID) instanceChanged()
    }, [resource.data, instanceID, instanceChanged])

    const columns = useMemo<Array<ColumnDef<DNSUpstream>>>(() => [
        {
            accessorKey: 'outbound',
            header: t('actualOutbound'),
            cell: ({ row }) => {
                const outbound = row.original.outbound ?? ''
                return <span className={outbound ? 'dns-upstream-outbound' : 'dns-muted'}>{outbound || t('unknownUpstream')}</span>
            },
        },
        { accessorKey: 'address', header: t('upstream'), cell: ({ row }) => <span className="dns-upstream-address">{row.original.address || t('unknownUpstream')}</span> },
        { accessorKey: 'attempts', header: t('attempts'), cell: ({ row }) => <span className="dns-num">{formatDNSNumber(row.original.attempts)}</span> },
        { accessorKey: 'successes', header: t('successes'), cell: ({ row }) => <span className="dns-num">{formatDNSNumber(row.original.successes)}</span> },
        { accessorKey: 'errors', header: t('errorsLabel'), cell: ({ row }) => <span className="dns-upstream-result"><span className={row.original.errors > 0 ? 'dns-num dns-error' : 'dns-num'}>{formatDNSNumber(row.original.errors)}</span>{row.original.errors > 0 && row.original.error_elapsed_ms_avg !== undefined && <small className="dns-upstream-wait">{t('meanWait')} {elapsed(row.original.error_elapsed_ms_avg, row.original.errors)}</small>}</span> },
        { accessorKey: 'canceled', header: t('canceled'), cell: ({ row }) => <span className="dns-upstream-result"><span className="dns-num">{formatDNSNumber(row.original.canceled)}</span>{row.original.canceled > 0 && row.original.canceled_elapsed_ms_avg !== undefined && <small className="dns-upstream-wait">{t('meanWait')} {elapsed(row.original.canceled_elapsed_ms_avg, row.original.canceled)}</small>}</span> },
        { accessorKey: 'timeouts', header: t('timeouts'), cell: ({ row }) => <span className="dns-num">{formatDNSNumber(row.original.timeouts)}</span> },
        { accessorKey: 'rcode_errors', header: t('rcodeErrors'), cell: ({ row }) => <span className="dns-num">{formatDNSNumber(row.original.rcode_errors)}</span> },
        { id: 'ratio', header: t('successRatio'), cell: ({ row }) => <span className="dns-num dns-strong">{cacheHitRatio(row.original.successes, row.original.successes + row.original.errors)}</span> },
        { id: 'latency', header: t(legacyLatency ? 'combinedAverageLatency' : 'successfulAverageLatency'), cell: ({ row }) => <span className="dns-num dns-strong">{legacyLatency ? elapsed(row.original.elapsed_ms_avg, row.original.successes + row.original.errors + row.original.canceled) : elapsed(row.original.success_elapsed_ms_avg, row.original.successes)}</span> },
    ], [t, legacyLatency])

    return <>
        <div className="dns-section-heading"><h2>{t('processScope')}</h2><DNSStatus {...resource} /></div>
        <div className="dns-captions">
            <p className="dns-muted dns-caption">{t('upstreamScope')}</p>
            <p className="dns-muted dns-caption">{t('successDefinition')}</p>
            <p className="dns-muted dns-caption">{t(legacyLatency ? 'legacyUpstreamLatency' : 'upstreamLatencyDefinition')}</p>
        </div>
        <Card className="dns-query-card">
            <DNSTable rows={data?.items ?? []} columns={columns} rowID={upstreamID} empty={resource.loading ? t('loading') : t('noUpstreams')} busy={resource.loading} />
        </Card>
        {data && <p className="dns-muted dns-caption">{t('upstreamLimit')}: {formatDNSNumber(data.limit)} · {t('upstreamOverflow')}: {formatDNSNumber(data.overflowed)}</p>}
    </>
}
