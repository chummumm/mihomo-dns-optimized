import { type ColumnDef } from '@tanstack/react-table'
import { AnimatePresence } from 'motion/react'
import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react'

import { Button, Card, Icon, Modal } from '@components'
import type { Client } from '@lib/request'

import { DNSStatus } from './Status'
import { DNSTable } from './Table'
import { formatDNSNumber, formatDNSTime } from './helpers'
import { useDNSI18n, useDNSResource } from './hooks'
import { cursorForPage, pageCount, readPageIndex, type PageIndex } from './pagination'
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
    const cached = query.cache === 'fresh' || query.cache === 'stale'
    const local = ['hosts', 'fake_ip', 'local', 'reject', 'drop'].includes(query.outcome)
    const noExchange = cached ? t('cacheNoExchange') : local ? t('localNoExchange') : t('unknownUpstream')
    const upstreams = query.upstreams ?? []
    const addresses = upstreams.map(source => `${source.address}${source.reused ? ` (${t('reusedAnswer')})` : ''}`)
    const exits = upstreams.map(source => `${source.outbound || t('unknownUpstream')}${source.reused ? ` (${t('reusedAnswer')})` : ''}`)

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
        [t('upstream'), addresses.length ? addresses.join('\n') : query.upstream || noExchange],
        [t('actualOutbound'), exits.length ? [...new Set(exits)].join('\n') : noExchange],
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
            {query.upstreams_truncated && <p className="dns-notice">{t('upstreamsTruncated')}</p>}
            <p className="dns-muted dns-caption">{t('upstreamAttribution')}</p>
            <div className="dns-details-footer"><Button onClick={close}>{t('close')}</Button></div>
        </div>
    </Modal>
}

interface PaginationProps {
    page: number
    total?: number
    pages?: number
    rows?: number
    limit: 50 | 100
    busy: boolean
    canNext: boolean
    go: (page: number) => void
    setLimit: (limit: 50 | 100) => void
}

function Pagination ({ page, total, pages, rows, limit, busy, canNext, go, setLimit }: PaginationProps) {
    const { t } = useDNSI18n()
    const [target, setTarget] = useState('')
    const parsed = /^[0-9]+$/.test(target.trim()) ? Number(target.trim()) : NaN
    const valid = Number.isSafeInteger(parsed) && parsed >= 1 && pages !== undefined && parsed <= pages && parsed !== page

    useEffect(() => { setTarget('') }, [page, limit])

    function jump (event: FormEvent) {
        event.preventDefault()
        if (valid && !busy) go(parsed)
    }

    return <div className="dns-pagination">
        <div className="dns-pagination-info">
            <span className="dns-pagination-position">
                {t('pagePrefix')} <b className="reading">{formatDNSNumber(page)}</b> / <b className="reading">{pages === undefined ? '—' : formatDNSNumber(pages)}</b> {t('pageSuffix')}
                <span className="dns-pagination-total">{' · '}{t('totalPrefix')} <b className="reading">{total === undefined ? '—' : formatDNSNumber(total)}</b> {t('totalSuffix')}</span>
            </span>
            <label>{t('pageSize')} <select className="dns-input" value={limit} onChange={event => setLimit(Number(event.target.value) as 50 | 100)}><option value="50">50</option><option value="100">100</option></select></label>
            {rows !== undefined && <span className="dns-muted">{t('pageRows')} {formatDNSNumber(rows)} {t('rows')}</span>}
        </div>
        <div className="dns-pagination-actions">
            <Button size="sm" icon="caret-line-left" disabled={page === 1 || busy} onClick={() => go(1)}>{t('first')}</Button>
            <Button size="sm" icon="caret-left" disabled={page === 1 || busy} onClick={() => go(page - 1)}>{t('previous')}</Button>
            <Button size="sm" className="dns-button-trailing" disabled={!canNext || busy} onClick={() => go(page + 1)}>{t('next')}<Icon type="caret-right" size={14} /></Button>
            <Button size="sm" className="dns-button-trailing" disabled={pages === undefined || page >= pages || busy} onClick={() => pages !== undefined && go(pages)}>{t('last')}<Icon type="caret-line-right" size={14} /></Button>
            <form className="dns-page-jump" onSubmit={jump}>
                <label>
                    <span>{t('jumpPrefix')}</span>
                    <input className="dns-input" value={target} inputMode="numeric" enterKeyHint="go" autoComplete="off"
                        aria-label={t('jumpLabel')} placeholder={String(page)} disabled={pages === undefined}
                        onChange={event => setTarget(event.target.value.replace(/[^0-9]/g, '').slice(0, 6))} />
                    <span>{t('jumpSuffix')}</span>
                </label>
                <Button size="sm" htmlType="submit" disabled={!valid || busy}>{t('jump')}</Button>
            </form>
        </div>
    </div>
}

interface Position { page: number, cursor: string, anchor?: string }
const latestPage: Position = { page: 1, cursor: '' }

export function DNSQueriesPage ({ client, instanceID, instanceChanged }: QueriesProps) {
    const { t } = useDNSI18n()
    const [draft, setDraft] = useState(emptyFilters)
    const [filters, setFilters] = useState(emptyFilters)
    const [limit, setLimit] = useState<50 | 100>(50)
    const [position, setPosition] = useState(latestPage)
    const [index, setIndex] = useState<{ scope: string, value?: PageIndex }>()
    const [legacyCursors, setLegacyCursors] = useState([''])
    const [live, setLive] = useState(false)
    const [selected, setSelected] = useState<DNSQuery>()
    const closeDetails = useCallback(() => setSelected(undefined), [])
    const tableRef = useRef<HTMLDivElement>(null)
    const scope = JSON.stringify([instanceID, limit, filters])
    const params = useMemo<DNSQueryParams>(() => ({
        limit,
        cursor: position.cursor || undefined,
        anchor: position.anchor || undefined,
        qname: filters.qname || undefined,
        client: filters.client || undefined,
        qtype: filters.qtype || undefined,
        outcome: filters.outcome || undefined,
    }), [position.cursor, position.anchor, filters, limit])
    const fetcher = useCallback(async (signal: AbortSignal) => (await client.getDNSQueries(params, signal)).data, [client, params])
    const resource = useDNSResource(fetcher, live && position.page === 1 ? 5000 : 0)
    const instanceMismatch = Boolean(resource.data && resource.data.instance_id !== instanceID)
    const data = resource.data?.instance_id === instanceID ? resource.data : undefined
    const responseIndex = useMemo(() => data && readPageIndex(data, limit), [data, limit])
    const current = instanceMismatch ? undefined : data ? responseIndex : index?.scope === scope ? index.value : undefined
    const total = current?.total
    const pages = total === undefined ? undefined : pageCount(total, limit)
    const outOfRange = pages !== undefined && position.page > pages

    useEffect(() => {
        if (!data) return
        setIndex({ scope, value: responseIndex })
        if (position.page === 1) setLegacyCursors([''])
        if (responseIndex && position.page > responseIndex.cursors.length) {
            // Eviction removes the oldest end of this anchored history. Clamp
            // once to its real last page; never report an empty invented page.
            setPosition({ page: responseIndex.cursors.length, cursor: responseIndex.cursors.at(-1) ?? '', anchor: responseIndex.anchor })
            setSelected(undefined)
        }
    }, [data, responseIndex, scope, position.page])

    useEffect(() => {
        tableRef.current?.querySelector('.dns-table-scroll')?.scrollTo({ top: 0 })
    }, [position])

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
        setPosition(latestPage)
        setLegacyCursors([''])
        setSelected(undefined)
    }

    function resetFilters () {
        setDraft(emptyFilters)
        setFilters(emptyFilters)
        setPosition(latestPage)
        setLegacyCursors([''])
        setSelected(undefined)
    }

    function changeLimit (value: 50 | 100) {
        setLimit(value)
        setPosition(latestPage)
        setLegacyCursors([''])
        setSelected(undefined)
    }

    function refresh () {
        // A history reduced to one page can still be refreshed back to latest.
        if (position.page === 1 && position.anchor) setPosition(latestPage)
        else resource.refresh()
    }

    function cursorFor (page: number) {
        if (page === 1) return ''
        if (current) return cursorForPage(current, page)
        if (page === position.page + 1 && data?.has_more && data.next_cursor && data.next_cursor !== position.cursor) return data.next_cursor
        return legacyCursors[page - 1]
    }

    function go (page: number) {
        if (resource.loading || outOfRange || !Number.isSafeInteger(page) || page < 1 || page === position.page || (pages !== undefined && page > pages)) return
        const cursor = cursorFor(page)
        if (cursor === undefined) return
        if (page > 1) {
            setLive(false)
            if (!current) setLegacyCursors(previous => [...previous.slice(0, page - 1), cursor])
        }
        setPosition(page === 1 ? latestPage : { page, cursor, anchor: current?.anchor || position.anchor })
        setSelected(undefined)
    }

    const canNext = Boolean(data?.has_more) && cursorFor(position.page + 1) !== undefined
    const history = position.page > 1

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
            <div className={live && !history ? 'dns-live-controls is-live' : 'dns-live-controls'}>
                <Button icon={live ? 'pause' : 'play'} pressed={live} disabled={history} onClick={() => { setLive(value => !value); if (!live && position.anchor) setPosition(latestPage) }}>{live ? t('pause') : t('startLive')}</Button>
                <span className="dns-live-dot" aria-hidden="true" />
                <span className="dns-muted dns-caption">{history ? t('historyPaused') : live ? t('liveHint') : t('pausedHint')}</span>
            </div>
            <DNSStatus {...resource} refresh={refresh} />
        </div>
        <Card className="dns-query-card">
            <div ref={tableRef}>
                <DNSTable rows={outOfRange ? [] : data?.items ?? []} columns={columns} rowID={queryID} empty={resource.loading || outOfRange ? t('loading') : t('noMatches')} busy={resource.loading || outOfRange} />
            </div>
            <Pagination key={scope} page={Math.min(position.page, pages ?? position.page)} total={total} pages={pages} rows={outOfRange ? undefined : data?.items.length}
                limit={limit} busy={resource.loading || outOfRange} canNext={canNext} go={go} setLimit={changeLimit} />
        </Card>
        {data && !responseIndex && <p className="dns-muted dns-caption">{t('paginationUnavailable')}</p>}
        <p className="dns-muted dns-caption">{t('retainedScope')} · {t('retained')}: {data ? formatDNSNumber(data.retained) : '—'} · {t('evicted')}: {data ? formatDNSNumber(data.evicted) : '—'}</p>
        {/* A new kernel identity discards old details immediately, including while capability refresh is failing. */}
        <AnimatePresence key={resource.data?.instance_id ?? instanceID}>{!instanceMismatch && selected && <QueryDetails key={selected.id} query={selected} close={closeDetails} />}</AnimatePresence>
    </>
}
