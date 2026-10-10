import { useReactTable, getSortedRowModel, getCoreRowModel, flexRender, createColumnHelper } from '@tanstack/react-table'
import classnames from 'classnames'
import { Fragment, useMemo, useLayoutEffect, useRef, useState, useEffect } from 'react'
import AutoSizer from 'react-virtualized-auto-sizer'

import { Header, Checkbox, Modal, Icon, Drawer, Card, Button, LineBullet, useLineColor } from '@components'
import { formatDateTime, fromNow } from '@lib/date'
import { basePath, formatTraffic } from '@lib/helper'
import { useObject, useVisible } from '@lib/hook'
import type * as API from '@lib/request'
import { useClient, useConnectionStreamReader, useI18n } from '@stores'

import { Devices } from './Devices'
import { ConnectionInfo } from './Info'
import { VirtualConnectionTable } from './VirtualTable'
import { formatConnection } from './helper'
import { ALL_CONNECTIONS, INTERNAL_CONNECTIONS } from './source'
import { type Connection, type FormatConnection, useConnections } from './store'
import './style.css'

const Columns = {
    Host: 'host',
    Network: 'network',
    Process: 'process',
    Type: 'type',
    Chains: 'chains',
    Rule: 'rule',
    Speed: 'speed',
    Upload: 'upload',
    Download: 'download',
    SourceIP: 'sourceIP',
    Time: 'time',
} as const

const shouldCenter = new Set<string>([Columns.Network, Columns.Type, Columns.Speed, Columns.Upload, Columns.Download, Columns.SourceIP, Columns.Time, Columns.Process])

function formatSpeed (upload: number, download: number) {
    switch (true) {
        case upload === 0 && download === 0:
            return '-'
        case upload !== 0 && download !== 0:
            return `↑ ${formatTraffic(upload)}/s ↓ ${formatTraffic(download)}/s`
        case upload !== 0:
            return `↑ ${formatTraffic(upload)}/s`
        default:
            return `↓ ${formatTraffic(download)}/s`
    }
}

const columnHelper = createColumnHelper<FormatConnection>()
const EMPTY_CONNECTIONS: Connection[] = []

export default function Connections () {
    const { translation, lang } = useI18n()
    const t = useMemo(() => translation('Connections').t, [translation])
    const connStreamReader = useConnectionStreamReader()
    const client = useClient()
    const color = useLineColor()
    const cardRef = useRef<HTMLDivElement>(null)

    // total
    const [traffic, setTraffic] = useObject({
        uploadTotal: 0,
        downloadTotal: 0,
    })

    // close all connections
    const { visible, show, hide } = useVisible()
    function handleCloseConnections () {
        client.closeAllConnections().finally(() => hide())
    }

    // connections
    const { connections, connectionsBySource, devices, feed, save, toggleSave, clearHistory, getConnection, historyLimit, discarded } = useConnections(connStreamReader)
    const [device, setDevice] = useState(ALL_CONNECTIONS)
    const selectedConnections = connectionsBySource.get(device) ?? EMPTY_CONNECTIONS
    const data = useMemo(() => selectedConnections.map(formatConnection), [selectedConnections])

    // table
    const columns = useMemo(
        () => [
            columnHelper.accessor(Columns.Host, { minSize: 260, size: 260, header: t(`columns.${Columns.Host}`) }),
            columnHelper.accessor(Columns.Network, { minSize: 80, size: 80, header: t(`columns.${Columns.Network}`) }),
            columnHelper.accessor(Columns.Type, { minSize: 100, size: 100, header: t(`columns.${Columns.Type}`), cell: cell => <span className={classnames('connections-type', { dns: cell.getValue() === 'DNS' })}>{cell.getValue()}</span> }),
            columnHelper.accessor(Columns.Chains, {
                minSize: 200,
                size: 200,
                header: t(`columns.${Columns.Chains}`),
                // One mark per hop, like interchange marks on a route: groups take their line bullet, nodes a small dot.
                // The separators stay in the text (visually hidden) so the cell text is the exact chain string.
                cell: cell => <span className="connections-route" title={cell.getValue()}>{cell.getValue().split(' / ').map((hop, index) => <Fragment key={index}>
                    {index > 0 && <span className="sr-only">{' / '}</span>}
                    {color(hop) === 'var(--idle)' ? <span className="connections-hop-dot" aria-hidden="true" /> : <LineBullet name={hop} />}
                    <span className="connections-hop">{hop}</span>
                </Fragment>)}</span>,
            }),
            columnHelper.accessor(Columns.Rule, { minSize: 140, size: 140, header: t(`columns.${Columns.Rule}`) }),
            columnHelper.accessor(Columns.Process, { minSize: 100, size: 100, header: t(`columns.${Columns.Process}`), cell: cell => cell.getValue() ? basePath(cell.getValue()!) : '-' }),
            columnHelper.accessor(
                row => [row.speed.upload, row.speed.download],
                {
                    id: Columns.Speed,
                    header: t(`columns.${Columns.Speed}`),
                    minSize: 200,
                    size: 200,
                    sortDescFirst: true,
                    sortingFn (rowA, rowB) {
                        const speedA = rowA.original?.speed ?? { upload: 0, download: 0 }
                        const speedB = rowB.original?.speed ?? { upload: 0, download: 0 }
                        return speedA.download === speedB.download
                            ? speedA.upload - speedB.upload
                            : speedA.download - speedB.download
                    },
                    cell: cell => {
                        const text = formatSpeed(cell.getValue()[0], cell.getValue()[1])
                        return <span className={classnames('connections-speed', { idle: text === '-' })}>{text}</span>
                    },
                },
            ),
            columnHelper.accessor(Columns.Upload, { minSize: 100, size: 100, header: t(`columns.${Columns.Upload}`), cell: cell => formatTraffic(cell.getValue()) }),
            columnHelper.accessor(Columns.Download, { minSize: 100, size: 100, header: t(`columns.${Columns.Download}`), cell: cell => formatTraffic(cell.getValue()) }),
            columnHelper.accessor(Columns.SourceIP, { minSize: 140, size: 140, header: t(`columns.${Columns.SourceIP}`) }),
            columnHelper.accessor(
                Columns.Time,
                {
                    minSize: 120,
                    size: 120,
                    header: t(`columns.${Columns.Time}`),
                    cell: cell => <time dateTime={cell.row.original.original.start} title={formatDateTime(new Date(cell.getValue()), lang)}>{fromNow(new Date(cell.getValue()), lang)}</time>,
                    sortingFn: (rowA, rowB) => (rowB.original?.time ?? 0) - (rowA.original?.time ?? 0),
                },
            ),
        ],
        [color, lang, t],
    )

    useLayoutEffect(() => {
        setTraffic({ uploadTotal: 0, downloadTotal: 0 })
        function handleConnection (snapshots: API.Snapshot[]) {
            for (const snapshot of snapshots) {
                setTraffic({
                    uploadTotal: snapshot.uploadTotal,
                    downloadTotal: snapshot.downloadTotal,
                })

                feed(snapshot.connections)
            }
        }

        connStreamReader?.subscribe('data', handleConnection)
        return () => {
            connStreamReader?.unsubscribe('data', handleConnection)
        }
    }, [connStreamReader, feed, setTraffic])

    useEffect(() => {
        setDevice(ALL_CONNECTIONS)
    }, [connStreamReader])

    useLayoutEffect(() => {
        if (device !== ALL_CONNECTIONS && !connectionsBySource.has(device)) setDevice(ALL_CONNECTIONS)
    }, [connectionsBySource, device])

    const instance = useReactTable({
        data,
        columns,
        getRowId: row => row.id,
        getCoreRowModel: getCoreRowModel(),
        getSortedRowModel: getSortedRowModel(),
        initialState: {
            sorting: [{ id: Columns.Time, desc: false }],
        },
        columnResizeMode: 'onChange',
        enableColumnResizing: true,
    })

    const headerGroup = instance.getHeaderGroups()[0]

    // click item
    const [drawerState, setDrawerState] = useObject({
        visible: false,
        selectedID: '',
        connection: {} as Partial<Connection>,
    })
    function handleConnectionClosed () {
        setDrawerState(d => { d.connection.completed = true })
        client.closeConnection(drawerState.selectedID)
    }
    useEffect(() => {
        const conn = getConnection(drawerState.selectedID)
        if (conn) {
            setDrawerState(d => { d.connection = conn })
        } else if (drawerState.selectedID) {
            setDrawerState(d => { d.connection.completed = true })
        }
    }, [connections, drawerState.selectedID, getConnection, setDrawerState])

    const headers = headerGroup.headers.map((header, idx) => {
        const column = header.column
        const id = column.id
        return (
            <div
                role="columnheader"
                className={classnames('connections-th', {
                    resizing: column.getIsResizing(),
                    fixed: column.id === Columns.Host,
                })}
                style={{ width: header.getSize() }}
                key={id}>
                <div className="connections-th-label" onClick={column.getToggleSortingHandler()}>
                    { flexRender(header.column.columnDef.header, header.getContext()) }
                    {
                        column.getIsSorted() !== false
                            ? <span className="connections-sort">
                                <span className="sr-only">{column.getIsSorted() === 'desc' ? ' ↓' : ' ↑'}</span>
                                <Icon type={column.getIsSorted() === 'desc' ? 'arrow-down' : 'arrow-up'} size={11} />
                            </span>
                            : null
                    }
                </div>
                { idx !== headerGroup.headers.length - 1 &&
                    <div
                        onMouseDown={header.getResizeHandler()}
                        onTouchStart={header.getResizeHandler()}
                        className="connections-resizer" />
                }
            </div>
        )
    })

    const rows = instance.getRowModel().rows

    // Escape closes the detail panel.
    useEffect(() => {
        if (!drawerState.visible) return
        function keydown (event: KeyboardEvent) {
            if (event.key === 'Escape') setDrawerState('visible', false)
        }
        document.addEventListener('keydown', keydown)
        return () => document.removeEventListener('keydown', keydown)
    }, [drawerState.visible, setDrawerState])

    const totals = (
        <span className="connections-traffic" title={t('total.text')}>
            <span className="connections-traffic-item" title={t('total.upload')}>
                <Icon type="arrow-up" size={12} className="connections-flow" replay={traffic.uploadTotal} />
                <span className="reading">{formatTraffic(traffic.uploadTotal)}</span>
            </span>
            <span className="connections-traffic-item" title={t('total.download')}>
                <Icon type="arrow-down" size={12} className="connections-flow" replay={traffic.downloadTotal} />
                <span className="reading">{formatTraffic(traffic.downloadTotal)}</span>
            </span>
        </span>
    )

    return (
        <div className="connections-page page">
            <Header title={t('title')} meta={totals}>
                <Checkbox className="connections-keep" checked={save} onChange={toggleSave}>{t('keepClosed')}</Checkbox>
                <Button className="connections-close-all" icon="close-all" onClick={show}>{t('closeAll.action')}</Button>
            </Header>
            {save && <div className="connections-history-note" role="status">
                <Icon type="info" size={14} className="connections-history-icon" />
                <span>{t('historyLimit')}: <span className="reading">{historyLimit}</span>. {t('retainedOnly')}{discarded > 0 && ' ' + t('historyDiscarded') + ': ' + discarded}</span>
                <Button type="ghost" size="sm" icon="trash" onClick={clearHistory}>{t('clearHistory')}</Button>
            </div>}
            <Devices devices={devices} total={connections.length} internalCount={connectionsBySource.get(INTERNAL_CONNECTIONS)?.length ?? 0} selected={device} onChange={setDevice} />
            <Card ref={cardRef} className="connections-card">
                <div className="connections-viewport">
                    <AutoSizer>
                        {({ height, width }) => <VirtualConnectionTable
                            rows={rows}
                            headers={headers}
                            width={width}
                            height={height}
                            totalWidth={instance.getTotalSize()}
                            centeredColumns={shouldCenter}
                            selectedID={drawerState.visible ? drawerState.selectedID : ''}
                            onSelect={id => setDrawerState({ visible: true, selectedID: id })} />}
                    </AutoSizer>
                    {data.length === 0 && <div className="connections-empty" role="status">
                        <Icon type="nav-connections" size={28} />
                        <span>{t('empty')}</span>
                    </div>}
                </div>
            </Card>
            <Modal title={t('closeAll.title')} show={visible} okType="danger" okText={t('closeAll.action')} onClose={hide} onOk={handleCloseConnections}>{t('closeAll.content')}</Modal>
            <Drawer containerRef={cardRef} bodyClassName="connections-drawer" visible={drawerState.visible} width={440} label={t('info.title')}>
                <div className="connections-drawer-head">
                    <span className="connections-drawer-title">{t('info.title')}</span>
                    <button type="button" className="connections-drawer-close" aria-label={t('info.closePanel')} onClick={() => setDrawerState('visible', false)}>
                        <Icon type="close" size={16} />
                    </button>
                </div>
                <ConnectionInfo className="connections-info" connection={drawerState.connection} />
                <div className="connections-drawer-foot">
                    <Button type="danger" icon="prohibit" disabled={drawerState.connection.completed} onClick={() => handleConnectionClosed()}>{ t('info.closeConnection') }</Button>
                </div>
            </Drawer>
        </div>
    )
}
