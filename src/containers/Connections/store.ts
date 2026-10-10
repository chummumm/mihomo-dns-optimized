import { useMemo, useSyncExternalStore } from 'react'

import type * as API from '@lib/request'

export type Connection = API.Connections & { completed?: boolean, uploadSpeed: number, downloadSpeed: number }

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
    label: string
    number: number
}

interface ConnectionState {
    connections: Connection[]
    devices: ConnectionDevice[]
    save: boolean
    discarded: number
    historyLimit: number
}

export class ConnectionsStore {
    private active = new Map<string, Connection>()
    private readonly closed = new Map<string, Connection>()
    private readonly deviceCounts = new Map<string, number>()
    private readonly listeners = new Set<() => void>()
    private closedCache: Connection[] = []
    private devicesCache: ConnectionDevice[] = []
    private closedChanged = false
    private devicesChanged = false
    private save = false
    private discarded = 0
    private receivedAt: number | undefined
    private state: ConnectionState

    constructor (private readonly historyLimit = CLOSED_CONNECTION_LIMIT) {
        if (!Number.isInteger(historyLimit) || historyLimit < 0) {
            throw new RangeError('Invalid closed connection limit')
        }
        this.state = { connections: [], devices: [], save: false, discarded: 0, historyLimit }
    }

    private changeDevice (sourceIP: string, delta: number) {
        const count = (this.deviceCounts.get(sourceIP) ?? 0) + delta
        if (count > 0) this.deviceCounts.set(sourceIP, count)
        else this.deviceCounts.delete(sourceIP)
        this.devicesChanged = true
    }

    private publish () {
        if (this.closedChanged) {
            this.closedCache = [...this.closed.values()]
            this.closedChanged = false
        }
        if (this.devicesChanged) {
            this.devicesCache = [...this.deviceCounts].map(([label, number]) => ({ label, number }))
                .sort((a, b) => a.label.localeCompare(b.label))
            this.devicesChanged = false
        }
        this.state = {
            connections: [...this.active.values(), ...this.closedCache],
            devices: this.devicesCache,
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
            if (retained === undefined) {
                this.changeDevice(connection.metadata.sourceIP, 1)
            } else if (retained.metadata.sourceIP !== connection.metadata.sourceIP) {
                this.changeDevice(retained.metadata.sourceIP, -1)
                this.changeDevice(connection.metadata.sourceIP, 1)
            }
            if (this.closed.delete(connection.id)) this.closedChanged = true
            next.set(connection.id, {
                ...connection,
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
                this.closedChanged = true
            } else {
                this.changeDevice(connection.metadata.sourceIP, -1)
            }
        }
        this.active = next

        while (this.closed.size > this.historyLimit) {
            const oldest = this.closed.entries().next().value as [string, Connection]
            this.closed.delete(oldest[0])
            this.changeDevice(oldest[1].metadata.sourceIP, -1)
            this.closedChanged = true
            this.discarded++
        }
        this.publish()
    }

    clearHistory = () => {
        for (const connection of this.closed.values()) this.changeDevice(connection.metadata.sourceIP, -1)
        this.closed.clear()
        this.closedChanged = true
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
