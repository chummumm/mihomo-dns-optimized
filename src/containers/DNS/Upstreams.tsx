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

export function DNSUpstreamPage ({ client, instanceID, instanceChanged }: UpstreamsProps) {
    const { t } = useDNSI18n()
    const fetcher = useCallback(async (signal: AbortSignal) => (await client.getDNSUpstreams(signal)).data, [client])
    const resource = useDNSResource(fetcher, 5000)
    const data = resource.data?.instance_id === instanceID ? resource.data : undefined

    useEffect(() => {
        if (resource.data && resource.data.instance_id !== instanceID) instanceChanged()
    }, [resource.data, instanceID, instanceChanged])

    const columns = useMemo<Array<ColumnDef<DNSUpstream>>>(() => [
        { accessorKey: 'address', header: t('upstream'), cell: ({ row }) => <span className="dns-upstream-address">{row.original.address || t('unknownUpstream')}</span> },
        { accessorKey: 'attempts', header: t('attempts'), cell: ({ row }) => formatDNSNumber(row.original.attempts) },
        { accessorKey: 'successes', header: t('successes'), cell: ({ row }) => formatDNSNumber(row.original.successes) },
        { accessorKey: 'errors', header: t('errorsLabel'), cell: ({ row }) => formatDNSNumber(row.original.errors) },
        { accessorKey: 'canceled', header: t('canceled'), cell: ({ row }) => formatDNSNumber(row.original.canceled) },
        { accessorKey: 'timeouts', header: t('timeouts'), cell: ({ row }) => formatDNSNumber(row.original.timeouts) },
        { accessorKey: 'rcode_errors', header: t('rcodeErrors'), cell: ({ row }) => formatDNSNumber(row.original.rcode_errors) },
        { id: 'ratio', header: t('successRatio'), cell: ({ row }) => cacheHitRatio(row.original.successes, row.original.successes + row.original.errors) },
        { accessorKey: 'elapsed_ms_avg', header: t('averageLatency'), cell: ({ row }) => `${formatDNSNumber(row.original.elapsed_ms_avg, 2)} ms` },
    ], [t])

    return <>
        <div className="dns-section-heading"><h2>{t('processScope')}</h2><DNSStatus {...resource} /></div>
        <p className="dns-muted dns-caption">{t('upstreamScope')}</p>
        <p className="dns-muted dns-caption">{t('successDefinition')}</p>
        <Card className="dns-query-card">
            <DNSTable rows={data?.items ?? []} columns={columns} rowID={upstreamID} empty={resource.loading ? t('loading') : t('noUpstreams')} busy={resource.loading} />
        </Card>
        {data && <p className="dns-muted dns-caption">{t('upstreamLimit')}: {formatDNSNumber(data.limit)} · {t('upstreamOverflow')}: {formatDNSNumber(data.overflowed)}</p>}
    </>
}
