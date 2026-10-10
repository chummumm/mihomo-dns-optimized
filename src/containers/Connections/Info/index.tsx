import classnames from 'classnames'
import { useMemo } from 'react'

import { formatDateTime } from '@lib/date'
import { basePath, formatTraffic } from '@lib/helper'
import { type BaseComponentProps } from '@models'
import { useI18n } from '@stores'

import { getConnectionHost, getConnectionPresentation, isDNSConnection } from '../helper'
import { type Connection } from '../store'

interface ConnectionsInfoProps extends BaseComponentProps {
    connection: Partial<Connection>
}

export function ConnectionInfo (props: ConnectionsInfoProps) {
    const { translation, lang } = useI18n()
    const t = useMemo(() => translation('Connections').t, [translation])
    const host = getConnectionHost(props.connection.metadata)
    const presentation = getConnectionPresentation(props.connection)

    return (
        <div className={classnames(props.className, 'flex flex-col overflow-y-auto text-sm')}>
            <div className="my-3 flex">
                <span className="w-20 font-bold">{t('info.id')}</span>
                <span className="flex-1 break-all font-mono">{props.connection.id}</span>
            </div>
            <div className="my-3 flex">
                <span className="w-20 font-bold">{t('info.start')}</span>
                <time className="flex-1 break-words font-mono" dateTime={props.connection.start}>{props.connection.start ? formatDateTime(new Date(props.connection.start), lang) : t('info.hostEmpty')}</time>
            </div>
            <div className="my-3 flex justify-between">
                <div className="flex flex-1">
                    <span className="w-20 font-bold">{t('info.network')}</span>
                    <span className="font-mono">{props.connection.metadata?.network}</span>
                </div>
                <div className="flex flex-1">
                    <span className="w-20 font-bold">{t('info.inbound')}</span>
                    <span className="font-mono">{presentation.type}</span>
                </div>
            </div>
            <div className="my-3 flex">
                <span className="w-20 font-bold">{t('info.host')}</span>
                <span className="flex-1 break-all font-mono">{
                    host
                        ? `${host}:${props.connection.metadata?.destinationPort}`
                        : t('info.hostEmpty')
                }</span>
            </div>
            <div className="my-3 flex">
                <span className="w-20 font-bold">{t('info.dstIP')}</span>
                <span className="flex-1 break-all font-mono">{
                    props.connection.metadata?.destinationIP
                        ? `${props.connection.metadata.destinationIP}:${props.connection.metadata?.destinationPort}`
                        : t('info.hostEmpty')
                }</span>
            </div>
            <div className="my-3 flex">
                <span className="w-20 font-bold">{t('info.srcIP')}</span>
                <span className="flex-1 break-all font-mono">{
                    props.connection.metadata?.sourceIP
                        ? `${props.connection.metadata.sourceIP}:${props.connection.metadata.sourcePort}`
                        : t('info.hostEmpty')
                }</span>
            </div>
            <div className="my-3 flex">
                <span className="w-20 font-bold">{t('info.process')}</span>
                <span className="flex-1 break-all font-mono">{
                    props.connection.metadata?.processPath
                        ? `${basePath(props.connection.metadata.processPath)}`
                        : t('info.hostEmpty')
                }</span>
            </div>
            <div className="my-3 flex">
                <span className="w-20 font-bold">{t('info.processPath')}</span>
                <span className="flex-1 break-all font-mono">{
                    props.connection.metadata?.processPath
                        ? `${props.connection.metadata.processPath}`
                        : t('info.hostEmpty')
                }</span>
            </div>
            {!isDNSConnection(props.connection) && <div className="my-3 flex">
                <span className="w-20 font-bold">{t('info.rule')}</span>
                <span className="flex-1 break-all font-mono">{presentation.rule}</span>
            </div>}
            <div className="my-3 flex">
                <span className="w-20 font-bold">{t('info.chains')}</span>
                <span className="flex-1 break-all font-mono">
                    {presentation.chains}
                </span>
            </div>
            <div className="my-3 flex justify-between">
                <div className="flex flex-1">
                    <span className="w-20 font-bold">{t('info.upload')}</span>
                    <span className="font-mono">{formatTraffic(props.connection.upload ?? 0)}</span>
                </div>
                <div className="flex flex-1">
                    <span className="w-20 font-bold">{t('info.download')}</span>
                    <span className="font-mono">{formatTraffic(props.connection.download ?? 0)}</span>
                </div>
            </div>
            <div className="my-3 flex">
                <span className="w-20 font-bold">{t('info.status')}</span>
                <span className="font-mono">{
                    !props.connection.completed
                        ? <span className="text-green">{t('info.opening')}</span>
                        : <span className="text-red">{t('info.closed')}</span>
                }</span>
            </div>
        </div>
    )
}
