import type { Connections } from '@lib/request'

import type { Connection, FormatConnection } from './store'

export function getConnectionHost (metadata?: Connections['metadata']) {
    if (metadata?.host) return metadata.host
    if (metadata?.sniffHost) return metadata.sniffHost
    return metadata?.destinationIP ?? ''
}

const formatted = new WeakMap<Connection, FormatConnection>()

export function formatConnection (connection: Connection): FormatConnection {
    const cached = formatted.get(connection)
    if (cached !== undefined) return cached
    const value = {
        id: connection.id,
        host: getConnectionHost(connection.metadata) + ':' + connection.metadata.destinationPort,
        chains: connection.chains.slice().reverse().join(' / '),
        rule: connection.rulePayload ? connection.rule + ' :: ' + connection.rulePayload : connection.rule,
        time: new Date(connection.start).getTime(),
        upload: connection.upload,
        download: connection.download,
        sourceIP: connection.metadata.sourceIP,
        type: connection.metadata.type,
        network: connection.metadata.network.toUpperCase(),
        process: connection.metadata.processPath,
        speed: { upload: connection.uploadSpeed, download: connection.downloadSpeed },
        completed: !!connection.completed,
        original: connection,
    }
    formatted.set(connection, value)
    return value
}
