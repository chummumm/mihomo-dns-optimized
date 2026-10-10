import type { Connections } from '@lib/request'

import type { Connection, FormatConnection } from './store'

export function getConnectionHost (metadata?: Connections['metadata']) {
    if (metadata?.host) return metadata.host
    if (metadata?.sniffHost) return metadata.sniffHost
    return metadata?.destinationIP ?? ''
}

export function isDNSConnection (connection: Partial<Connections>) {
    return connection.dns === true
}

export function getConnectionPresentation (connection: Partial<Connections>) {
    const dns = isDNSConnection(connection)
    return {
        type: dns ? 'DNS' : connection.metadata?.type ?? '',
        // Mihomo records chains from the final adapter to outer proxy groups.
        chains: dns ? connection.chains?.[0] ?? '' : connection.chains?.slice().reverse().join(' / ') ?? '',
        rule: dns ? '' : connection.rulePayload ? `${connection.rule} :: ${connection.rulePayload}` : connection.rule ?? '',
    }
}

const formatted = new WeakMap<Connection, FormatConnection>()

export function formatConnection (connection: Connection): FormatConnection {
    const cached = formatted.get(connection)
    if (cached !== undefined) return cached
    const value = {
        ...getConnectionPresentation(connection),
        id: connection.id,
        host: getConnectionHost(connection.metadata) + ':' + connection.metadata.destinationPort,
        time: new Date(connection.start).getTime(),
        upload: connection.upload,
        download: connection.download,
        sourceIP: connection.metadata.sourceIP,
        network: connection.metadata.network.toUpperCase(),
        process: connection.metadata.processPath,
        speed: { upload: connection.uploadSpeed, download: connection.downloadSpeed },
        completed: !!connection.completed,
        original: connection,
    }
    formatted.set(connection, value)
    return value
}
