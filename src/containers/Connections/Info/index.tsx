import classnames from 'classnames'
import { useMemo, type ReactNode } from 'react'

import { LineBullet, useLineColor } from '@components'
import { formatDateTime } from '@lib/date'
import { basePath, formatTraffic } from '@lib/helper'
import { type BaseComponentProps } from '@models'
import { useI18n } from '@stores'

import { getConnectionHost, getConnectionPresentation, isDNSConnection } from '../helper'
import { type Connection } from '../store'

interface ConnectionsInfoProps extends BaseComponentProps {
    connection: Partial<Connection>
}

function Stop ({ label, children, kind, line, routed }: { label: string, children: ReactNode, kind?: 'origin' | 'end', line?: string, routed?: boolean }) {
    return (
        <li className={classnames('journey-stop', kind && `journey-${kind}`, { 'is-routed': routed })}>
            {line ? <LineBullet name={line} className="journey-dot" /> : <span className="journey-dot" aria-hidden="true" />}
            <span className="journey-label">{label}</span>
            <div className="journey-value">{children}</div>
        </li>
    )
}

// The connection drawn as a journey: where it came from, how it was routed, where it went.
export function ConnectionInfo (props: ConnectionsInfoProps) {
    const { translation, lang } = useI18n()
    const t = useMemo(() => translation('Connections').t, [translation])
    const host = getConnectionHost(props.connection.metadata)
    const presentation = getConnectionPresentation(props.connection)
    const dns = isDNSConnection(props.connection)
    const metadata = props.connection.metadata
    const firstLine = presentation.chains.split(' / ')[0]
    const color = useLineColor()

    return (
        <div className={classnames(props.className, 'connection-info')}>
            <div className="connection-status">
                {
                    !props.connection.completed
                        ? <span className="connection-state is-open">{t('info.opening')}</span>
                        : <span className="connection-state is-closed">{t('info.closed')}</span>
                }
                <span className="connection-network mono">{metadata?.network}</span>
            </div>

            <ol className="journey" style={{ '--line': color(firstLine) } as React.CSSProperties}>
                <Stop label={t('info.srcIP')} kind="origin">
                    <span className="mono">{
                        metadata?.sourceIP
                            ? `${metadata.sourceIP}:${metadata.sourcePort}`
                            : t('info.hostEmpty')
                    }</span>
                </Stop>
                <Stop label={t('info.inbound')}>
                    <span className="mono">{presentation.type}</span>
                </Stop>
                {!dns && <Stop label={t('info.rule')}>
                    <span className="mono">{presentation.rule}</span>
                </Stop>}
                <Stop label={t('info.chains')} line={firstLine} routed>
                    <span className="journey-chains mono">{presentation.chains}</span>
                </Stop>
                <Stop label={t('info.host')} kind="end">
                    <span className="mono journey-strong">{
                        host
                            ? `${host}:${metadata?.destinationPort}`
                            : t('info.hostEmpty')
                    }</span>
                </Stop>
            </ol>

            <dl className="connection-facts">
                <div>
                    <dt>{t('info.dstIP')}</dt>
                    <dd className="mono">{
                        metadata?.destinationIP
                            ? `${metadata.destinationIP}:${metadata?.destinationPort}`
                            : t('info.hostEmpty')
                    }</dd>
                </div>
                <div className="connection-facts-pair">
                    <div>
                        <dt>{t('info.upload')}</dt>
                        <dd className="mono">{formatTraffic(props.connection.upload ?? 0)}</dd>
                    </div>
                    <div>
                        <dt>{t('info.download')}</dt>
                        <dd className="mono">{formatTraffic(props.connection.download ?? 0)}</dd>
                    </div>
                </div>
                <div>
                    <dt>{t('info.start')}</dt>
                    <dd><time className="mono" dateTime={props.connection.start}>{props.connection.start ? formatDateTime(new Date(props.connection.start), lang) : t('info.hostEmpty')}</time></dd>
                </div>
                <div>
                    <dt>{t('info.process')}</dt>
                    <dd className="mono">{
                        metadata?.processPath
                            ? `${basePath(metadata.processPath)}`
                            : t('info.hostEmpty')
                    }</dd>
                </div>
                <div>
                    <dt>{t('info.processPath')}</dt>
                    <dd className="mono">{
                        metadata?.processPath
                            ? `${metadata.processPath}`
                            : t('info.hostEmpty')
                    }</dd>
                </div>
                <div>
                    <dt>{t('info.id')}</dt>
                    <dd className="mono">{props.connection.id}</dd>
                </div>
            </dl>
        </div>
    )
}
