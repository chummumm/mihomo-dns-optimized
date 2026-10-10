import { type ColumnDef } from '@tanstack/react-table'
import { useCallback, useEffect, useMemo, useState, type FormEvent } from 'react'

import { Button, Card, Icon, Modal } from '@components'
import type { Client } from '@lib/request'

import { DNSStatus } from './Status'
import { DNSTable } from './Table'
import { formatDNSNumber, formatDNSTime } from './helpers'
import { useDNSI18n, useDNSResource } from './hooks'
import type { DNSOutcome, DNSQuery, DNSQueryParams } from './types'

const outcomes: DNSOutcome[] = ['cache_fresh', 'cache_stale', 'hosts', 'fake_ip', 'upstream', 'reject', 'drop', 'error', 'local']
const emptyFilters = { qname: '', client: '', qtype: '', outcome: '' as DNSOutcome | '' }
const queryID = (query: DNSQuery) => query.id

interface QueriesProps {
    client: Client
    instanceID: string
    instanceChanged: () => void
}

function QueryDetails ({ query, close }: { query: DNSQuery, close: () => void }) {
    const { t } = useDNSI18n()

    useEffect(() => {
        function keydown (event: KeyboardEvent) {
            if (event.key === 'Escape') close()
        }
        document.addEventListener('keydown', keydown)
        return () => document.removeEventListener('keydown', keydown)
    }, [close])

    const fields = [
        [t('time'), formatDNSTime(query.time, true)],
        [t('domain'), query.qname],
        [t('client'), query.client === 'unknown' ? t('unknown') : query.client],
        [t('queryType'), query.qtype],
        [t('protocol'), query.protocol === 'unknown' ? t('unknown') : query.protocol],
        [t('source'), query.source === 'unknown' ? t('unknown') : query.source],
        [t('outcomeLabel'), t(`outcome.${query.outcome}`)],
        [t('cache'), t(`cacheValue.${query.cache}`)],
        ['RCODE', query.rcode || t('noResponse')],
        [t('latency'), `${formatDNSNumber(query.elapsed_ms, 2)} ms`],
        [t('upstream'), query.upstream || t('unknownUpstream')],
        [t('errorClass'), query.error || '—'],
    ]

    return <Modal title={t('details')} size="big" show footer={false} onClose={close} bodyClassName="dns-details">
        <div role="dialog" aria-label={t('details')} aria-modal="true">
            <dl className="dns-details-fields">
                {fields.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}
            </dl>
            <h3>{t('answers')}</h3>
            {query.answers.length === 0
                ? <p className="dns-muted">{t('noAnswers')}</p>
                : <div className="dns-table-scroll">
                    <table className="dns-table dns-answer-table">
                        <thead><tr><th scope="col">{t('queryType')}</th><th scope="col">{t('value')}</th><th scope="col">TTL (s)</th></tr></thead>
                        <tbody>{query.answers.map((answer, index) => <tr key={`${index}-${answer.type}-${answer.value}`}>
                            <td>{answer.type}</td><td className="dns-answer-value">{answer.value}</td><td>{formatDNSNumber(answer.ttl)}</td>
                        </tr>)}</tbody>
                    </table>
                </div>}
            {query.answers_truncated && <p className="dns-notice">{t('answersTruncated')}</p>}
            <p className="dns-muted dns-caption">{t('upstreamAttribution')}</p>
            <div className="dns-details-footer"><Button onClick={close}>{t('close')}</Button></div>
        </div>
    </Modal>
}

export function DNSQueriesPage ({ client, instanceID, instanceChanged }: QueriesProps) {
    const { t } = useDNSI18n()
    const [draft, setDraft] = useState(emptyFilters)
    const [filters, setFilters] = useState(emptyFilters)
    const [limit, setLimit] = useState<50 | 100>(50)
    const [cursors, setCursors] = useState([''])
    const [live, setLive] = useState(false)
    const [selected, setSelected] = useState<DNSQuery>()
    const closeDetails = useCallback(() => setSelected(undefined), [])
    const cursor = cursors[cursors.length - 1]
    const params = useMemo<DNSQueryParams>(() => ({
        limit,
        cursor: cursor || undefined,
        qname: filters.qname || undefined,
        client: filters.client || undefined,
        qtype: filters.qtype || undefined,
        outcome: filters.outcome || undefined,
    }), [cursor, filters, limit])
    const fetcher = useCallback(async (signal: AbortSignal) => (await client.getDNSQueries(params, signal)).data, [client, params])
    const resource = useDNSResource(fetcher, live && cursors.length === 1 ? 5000 : 0)
    const data = resource.data?.instance_id === instanceID ? resource.data : undefined

    useEffect(() => {
        if (resource.data && resource.data.instance_id !== instanceID) {
            setSelected(undefined)
            instanceChanged()
        }
    }, [resource.data, instanceID, instanceChanged])

    const columns = useMemo<Array<ColumnDef<DNSQuery>>>(() => [
        { accessorKey: 'time', header: t('time'), cell: ({ row }) => <span className="dns-time" title={formatDNSTime(row.original.time, true)}>{formatDNSTime(row.original.time)}</span> },
        { accessorKey: 'qname', header: t('domain'), cell: ({ row }) => <button className="dns-domain-button" title={row.original.qname} onClick={() => setSelected(row.original)}>{row.original.qname}</button> },
        { accessorKey: 'qtype', header: t('queryType'), cell: ({ row }) => <span className="dns-qtype">{row.original.qtype}</span> },
        { accessorKey: 'client', header: t('client'), cell: ({ row }) => row.original.client === 'unknown' ? t('unknown') : row.original.client },
        { accessorKey: 'outcome', header: t('outcomeLabel'), cell: ({ row }) => <span className={`dns-outcome dns-outcome-${row.original.outcome}`}>{t(`outcome.${row.original.outcome}`)}</span> },
        { accessorKey: 'rcode', header: 'RCODE', cell: ({ row }) => row.original.rcode || '—' },
        { accessorKey: 'elapsed_ms', header: t('latency'), cell: ({ row }) => <span className="dns-num">{`${formatDNSNumber(row.original.elapsed_ms, 2)} ms`}</span> },
    ], [t])

    function applyFilters (event: FormEvent) {
        event.preventDefault()
        setFilters({ ...draft, qname: draft.qname.trim(), client: draft.client.trim(), qtype: draft.qtype.trim().toUpperCase() })
        setCursors([''])
        setSelected(undefined)
    }

    function resetFilters () {
        setDraft(emptyFilters)
        setFilters(emptyFilters)
        setCursors([''])
        setSelected(undefined)
    }

    function nextPage () {
        if (data?.has_more && data.next_cursor && data.next_cursor !== cursor) {
            setLive(false)
            setCursors(previous => [...previous, data.next_cursor])
        }
    }

    return <>
        <Card className="dns-section">
            <form className="dns-filters" onSubmit={applyFilters}>
                <label className="dns-field dns-field-wide">{t('domain')}<input className="dns-input" value={draft.qname} maxLength={254} placeholder={t('exactDomain')} spellCheck={false} onChange={event => setDraft(previous => ({ ...previous, qname: event.target.value }))} /></label>
                <label className="dns-field">{t('client')}<input className="dns-input" value={draft.client} maxLength={64} placeholder={t('exactClient')} spellCheck={false} onChange={event => setDraft(previous => ({ ...previous, client: event.target.value }))} /></label>
                <label className="dns-field dns-field-narrow">{t('queryType')}<input className="dns-input dns-type-input" value={draft.qtype} maxLength={16} placeholder="A / AAAA" spellCheck={false} onChange={event => setDraft(previous => ({ ...previous, qtype: event.target.value }))} /></label>
                <label className="dns-field">{t('outcomeLabel')}<select aria-label={t('outcomeLabel')} className="dns-input" value={draft.outcome} onChange={event => setDraft(previous => ({ ...previous, outcome: event.target.value as DNSOutcome | '' }))}>
                    <option value="">{t('all')}</option>
                    {outcomes.map(outcome => <option key={outcome} value={outcome}>{t(`outcome.${outcome}`)}</option>)}
                </select></label>
                <div className="dns-filter-actions"><button className="button button-primary" type="submit"><Icon type="search" size={15} />{t('apply')}</button><button className="button button-normal" type="button" onClick={resetFilters}>{t('reset')}</button></div>
            </form>
            <p className="dns-muted dns-caption">{t('filterHint')}</p>
        </Card>
        <div className="dns-section-heading">
            <div className={live && cursors.length === 1 ? 'dns-live-controls is-live' : 'dns-live-controls'}>
                <Button icon={live ? 'pause' : 'play'} pressed={live} disabled={cursors.length > 1} onClick={() => setLive(value => !value)}>{live ? t('pause') : t('startLive')}</Button>
                <span className="dns-live-dot" aria-hidden="true" />
                <span className="dns-muted dns-caption">{cursors.length > 1 ? t('historyPaused') : live ? t('liveHint') : t('pausedHint')}</span>
            </div>
            <DNSStatus {...resource} />
        </div>
        <Card className="dns-query-card">
            <DNSTable rows={data?.items ?? []} columns={columns} rowID={queryID} empty={resource.loading ? t('loading') : t('noMatches')} busy={resource.loading} />
            <div className="dns-pagination">
                <div className="dns-pagination-info">
                    <span>{t('page')} <b className="reading">{cursors.length}</b> · {data ? formatDNSNumber(data.items.length) : '—'} {t('rows')}</span>
                    <label>{t('pageSize')} <select className="dns-input" value={limit} onChange={event => { setLimit(Number(event.target.value) as 50 | 100); setCursors(['']) }}><option value="50">50</option><option value="100">100</option></select></label>
                </div>
                <div className="dns-pagination-actions">
                    <Button size="sm" disabled={cursors.length === 1 || resource.loading} onClick={() => setCursors([''])}>{t('latest')}</Button>
                    <Button size="sm" icon="caret-left" disabled={cursors.length === 1 || resource.loading} onClick={() => setCursors(previous => previous.slice(0, -1))}>{t('previous')}</Button>
                    <Button size="sm" disabled={!data?.has_more || !data.next_cursor || resource.loading} onClick={nextPage}>{t('next')}<Icon type="caret-right" size={14} /></Button>
                </div>
            </div>
        </Card>
        <p className="dns-muted dns-caption">{t('retainedScope')} · {t('retained')}: {data ? formatDNSNumber(data.retained) : '—'} · {t('evicted')}: {data ? formatDNSNumber(data.evicted) : '—'}</p>
        {selected && <QueryDetails query={selected} close={closeDetails} />}
    </>
}
