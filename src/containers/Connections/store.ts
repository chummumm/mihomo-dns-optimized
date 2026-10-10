import { useMemo, useSyncExternalStore } from 'react'

import type * as API from '@lib/request'

import { ALL_CONNECTIONS, getConnectionSourceKey } from './source'

export type Connection = API.Connections & { sourceKey: string, completed?: boolean, uploadSpeed: number, downloadSpeed: number }

export interface FormatConnection {
    id: string
    host: string
    chains: string
    rule: string
    time: number
    upload: number
    download: number
    type: string
    network: string
    process?: string
    sourceIP: string
    speed: {
        upload: number
        download: number
    }
    completed: boolean
    original: Connection
}

export const CLOSED_CONNECTION_LIMIT = 5000

export interface ConnectionDevice {
    key: string
    label: string
    number: number
}

interface ConnectionState {
    connections: Connection[]
    connectionsBySource: ReadonlyMap<string, Connection[]>
    devices: ConnectionDevice[]
    save: boolean
    discarded: number
    historyLimit: number
}

export class ConnectionsStore {
    private active = new Map<string, Connection>()
    private readonly closed = new Map<string, Connection>()
    private readonly listeners = new Set<() => void>()
    private save = false
    private discarded = 0
    private receivedAt: number | undefined
    private state: ConnectionState

    constructor (private readonly historyLimit = CLOSED_CONNECTION_LIMIT) {
        if (!Number.isInteger(historyLimit) || historyLimit < 0) {
            throw new RangeError('Invalid closed connection limit')
        }
        const connections: Connection[] = []
        this.state = { connections, connectionsBySource: new Map([[ALL_CONNECTIONS, connections]]), devices: [], save: false, discarded: 0, historyLimit }
    }

    private publish () {
        const connections: Connection[] = []
        const connectionsBySource = new Map<string, Connection[]>()
        // Keep source buttons in stable order without sorting every snapshot.
        for (const device of this.state.devices) connectionsBySource.set(device.key, [])
        // Each record is visited once and shared by All and exactly one source.
        // Counters and filters use these same buckets, never independent counts.
        for (const collection of [this.active, this.closed]) {
            for (const connection of collection.values()) {
                connections.push(connection)
                const bucket = connectionsBySource.get(connection.sourceKey)
                if (bucket) bucket.push(connection)
                else connectionsBySource.set(connection.sourceKey, [connection])
            }
        }
        const devices: ConnectionDevice[] = []
        for (const [key, bucket] of connectionsBySource) {
            if (bucket.length === 0) connectionsBySource.delete(key)
            else devices.push({ key, label: key.startsWith('device:') ? key.slice(7) : key, number: bucket.length })
        }
        connectionsBySource.set(ALL_CONNECTIONS, connections)
        this.state = {
            connections,
            connectionsBySource,
            devices,
            save: this.save,
            discarded: this.discarded,
            historyLimit: this.historyLimit,
        }
        for (const listener of this.listeners) listener()
    }

    feed = (connections: API.Connections[] | null, now = performance.now()) => {
        if (connections !== null && !Array.isArray(connections)) {
            throw new TypeError('Invalid connection snapshot')
        }
        const elapsed = this.receivedAt === undefined ? 0 : (now - this.receivedAt) / 1000
        this.receivedAt = now
        const next = new Map<string, Connection>()

        for (const connection of connections ?? []) {
            if (next.has(connection.id)) continue
            const previous = this.active.get(connection.id)
            const retained = previous ?? this.closed.get(connection.id)
            const sourceKey = retained !== undefined && retained.metadata?.sourceIP === connection.metadata?.sourceIP &&
                retained.metadata?.type === connection.metadata?.type && (retained.dns === true) === (connection.dns === true)
                ? retained.sourceKey
                : getConnectionSourceKey(connection)
            this.closed.delete(connection.id)
            next.set(connection.id, {
                ...connection,
                sourceKey,
                completed: false,
                uploadSpeed: previous !== undefined && elapsed > 0 ? Math.max(0, connection.upload - previous.upload) / elapsed : 0,
                downloadSpeed: previous !== undefined && elapsed > 0 ? Math.max(0, connection.download - previous.download) / elapsed : 0,
            })
        }

        // Only last snapshot's active connections can have just closed. Retained
        // history is indexed separately and is never reclassified on each tick.
        for (const [id, connection] of this.active) {
            if (next.has(id)) continue
            if (this.save) {
                this.closed.set(id, { ...connection, completed: true, uploadSpeed: 0, downloadSpeed: 0 })
            }
        }
        this.active = next

        while (this.closed.size > this.historyLimit) {
            const oldest = this.closed.keys().next().value as string
            this.closed.delete(oldest)
            this.discarded++
        }
        this.publish()
    }

    clearHistory = () => {
        this.closed.clear()
        this.discarded = 0
        this.publish()
    }

    toggleSave = () => {
        this.save = !this.save
        if (!this.save) this.clearHistory()
        else this.publish()
    }

    getConnection = (id: string) => this.active.get(id) ?? this.closed.get(id)

    getSnapshot = () => this.state

    subscribe = (listener: () => void) => {
        this.listeners.add(listener)
        return () => { this.listeners.delete(listener) }
    }
}

export function useConnections (scope: unknown) {
    const { store } = useMemo(() => ({ scope, store: new ConnectionsStore() }), [scope])
    const state = useSyncExternalStore(store.subscribe, store.getSnapshot)
    return { ...state, feed: store.feed, toggleSave: store.toggleSave, clearHistory: store.clearHistory, getConnection: store.getConnection }
}
