const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { createRequire } = require('node:module')
const { test } = require('node:test')
const ts = require('typescript')

function loadSource(relative) {
    const filename = path.resolve(__dirname, relative)
    const compiled = ts.transpileModule(fs.readFileSync(filename, 'utf8'), {
        compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020, esModuleInterop: true },
    }).outputText
    const module = { exports: {} }
    new Function('require', 'module', 'exports', compiled)(createRequire(filename), module, module.exports)
    return module.exports
}

const { ConnectionsStore } = loadSource('store.ts')
const { StreamReader } = loadSource('../../lib/streamer.ts')

function connection(id, sourceIP = '192.0.2.1', download = 0) {
    return {
        id,
        metadata: { sourceIP, sourcePort: '50000', host: id + '.test', destinationIP: '198.51.100.1', destinationPort: '443', network: 'tcp', type: 'Socks5' },
        upload: 0, download, start: '2026-01-01T00:00:00Z', chains: ['DIRECT'], rule: 'Match', rulePayload: '',
    }
}

function ids(store) {
    return store.getSnapshot().connections.map(value => value.id).sort()
}

function assertCounts(store, expected) {
    const state = store.getSnapshot()
    assert.deepEqual(Object.fromEntries(state.devices.map(value => [value.label, value.number])), expected)
    assert.equal(state.connections.length, Object.values(expected).reduce((total, count) => total + count, 0))
    assert.equal(new Set(state.connections.map(value => value.id)).size, state.connections.length)
}

test('every valid snapshot updates the observed collection, including null and empty', () => {
    const store = new ConnectionsStore()
    const frames = []
    store.subscribe(() => frames.push(ids(store)))
    store.feed([connection('A')], 1000)
    store.feed([connection('A'), connection('B')], 2000)
    store.feed([connection('A')], 3000)
    store.feed(null, 4000)
    store.feed([], 5000)
    assert.deepEqual(frames, [['A'], ['A', 'B'], ['A'], [], []])
    assertCounts(store, {})
    const before = store.getSnapshot()
    assert.throws(() => store.feed({}), /Invalid connection snapshot/)
    assert.equal(store.getSnapshot(), before)
})

test('retained closed records share counts with active records and are evicted oldest first', () => {
    const store = new ConnectionsStore(2)
    store.toggleSave()
    store.feed([connection('A'), connection('B'), connection('C', '192.0.2.2')], 1000)
    store.feed([connection('D', '192.0.2.2')], 2000)
    assert.deepEqual(ids(store), ['B', 'C', 'D'])
    assert.equal(store.getSnapshot().discarded, 1)
    assert.equal(store.getConnection('B').completed, true)
    assert.equal(store.getConnection('D').completed, false)
    assertCounts(store, { '192.0.2.1': 1, '192.0.2.2': 2 })

    const closed = store.getConnection('B')
    store.feed([connection('D', '192.0.2.2', 10)], 3000)
    assert.equal(store.getConnection('B'), closed, 'unchanged history is reused')
    assert.equal(store.getSnapshot().discarded, 1)
})

test('history limits never discard active connections', () => {
    const store = new ConnectionsStore(2)
    store.toggleSave()
    store.feed([connection('old')], 1000)
    const active = Array.from({ length: 200 }, (_, i) => connection('active-' + i))
    store.feed(active, 2000)
    assert.equal(store.getSnapshot().connections.length, 201)
    assert.equal(store.getSnapshot().connections.filter(value => !value.completed).length, 200)
    assertCounts(store, { '192.0.2.1': 201 })
    store.feed(null, 3000)
    assert.equal(store.getSnapshot().connections.length, 2)
    assert.equal(store.getSnapshot().discarded, 199)
    assertCounts(store, { '192.0.2.1': 2 })
})

test('reappearing IDs and source changes do not duplicate or lose device counts', () => {
    const store = new ConnectionsStore(4)
    store.toggleSave()
    store.feed([connection('A')], 1000)
    store.feed(null, 2000)
    store.feed([connection('A', '192.0.2.2'), connection('A', '192.0.2.2')], 3000)
    assert.deepEqual(ids(store), ['A'])
    assert.equal(store.getConnection('A').completed, false)
    assertCounts(store, { '192.0.2.2': 1 })
    store.feed([connection('A', '192.0.2.3')], 4000)
    assertCounts(store, { '192.0.2.3': 1 })
})

test('clearing or disabling history preserves current active connections', () => {
    const store = new ConnectionsStore(2)
    store.toggleSave()
    store.feed([connection('A'), connection('B')], 1000)
    store.feed([connection('B')], 2000)
    store.clearHistory()
    assert.deepEqual(ids(store), ['B'])
    assert.equal(store.getSnapshot().save, true)
    assertCounts(store, { '192.0.2.1': 1 })
    store.feed(null, 3000)
    store.toggleSave()
    assert.deepEqual(ids(store), [])
    assert.equal(store.getSnapshot().save, false)
    assertCounts(store, {})
})

test('speed uses elapsed time and never becomes negative after a counter reset', () => {
    const store = new ConnectionsStore()
    store.feed([connection('A', '192.0.2.1', 100)], 1000)
    store.feed([connection('A', '192.0.2.1', 150)], 3000)
    assert.equal(store.getConnection('A').downloadSpeed, 25)
    store.feed([connection('A', '192.0.2.1', 10)], 4000)
    assert.equal(store.getConnection('A').downloadSpeed, 0)
})

test('stream reader reconnects safely without retaining connection snapshots', async () => {
    const original = global.WebSocket
    const sockets = []
    class FakeSocket {
        static CLOSING = 2
        readyState = 1
        handlers = new Map()
        constructor(url) { this.url = url; sockets.push(this) }
        addEventListener(type, handler) {
            const handlers = this.handlers.get(type) || []
            handlers.push(handler); this.handlers.set(type, handlers)
        }
        emit(type, event = {}) { for (const handler of this.handlers.get(type) || []) handler(event) }
        close() { this.readyState = 3; this.emit('close') }
    }
    global.WebSocket = FakeSocket
    const reader = new StreamReader({ bufferLength: 0, retryInterval: 5 })
    const received = []
    try {
        reader.subscribe('data', values => received.push(...values))
        reader.connect('ws://127.0.0.1:1/connections')
        const first = sockets[0]
        first.emit('message', { data: JSON.stringify({ connections: null }) })
        first.emit('message', { data: '{invalid' })
        assert.equal(received.length, 1)
        assert.deepEqual(reader.buffer(), [])
        first.close()
        await new Promise(resolve => setTimeout(resolve, 20))
        assert.equal(sockets.length, 2)
        reader.connect('ws://127.0.0.1:2/connections')
        first.emit('error', new Error('late error'))
        assert.equal(sockets[2].readyState, 1)
        sockets[2].close()
        reader.destory()
        await new Promise(resolve => setTimeout(resolve, 20))
        assert.equal(sockets.length, 3, 'destroy cancels pending retries')
        first.emit('message', { data: JSON.stringify({ connections: [] }) })
        assert.equal(received.length, 1, 'obsolete sockets cannot deliver data')
    } finally {
        reader.destory()
        global.WebSocket = original
    }
})
