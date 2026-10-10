const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { createRequire } = require('node:module')
const { test } = require('node:test')
const ts = require('typescript')

const loadedSources = new Map()
function loadSource(relative) {
    const filename = path.resolve(__dirname, relative)
    if (loadedSources.has(filename)) return loadedSources.get(filename).exports
    const compiled = ts.transpileModule(fs.readFileSync(filename, 'utf8'), {
        compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020, esModuleInterop: true },
    }).outputText
    const module = { exports: {} }
    loadedSources.set(filename, module)
    const requireModule = createRequire(filename)
    const localRequire = name => {
        const local = path.resolve(path.dirname(filename), name + '.ts')
        return name.startsWith('.') && fs.existsSync(local) ? loadSource(local) : requireModule(name)
    }
    new Function('require', 'module', 'exports', compiled)(localRequire, module, module.exports)
    return module.exports
}

const { ConnectionsStore } = loadSource('store.ts')
const { StreamReader } = loadSource('../../lib/streamer.ts')
const { formatConnection, getConnectionPresentation, isDNSConnection } = loadSource('helper.ts')
const { getConnectionSourceKey, ALL_CONNECTIONS, INTERNAL_CONNECTIONS, UNKNOWN_CONNECTIONS } = loadSource('source.ts')

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
    assert.equal(state.connectionsBySource.get(ALL_CONNECTIONS), state.connections, 'All shares the published collection')
    const grouped = []
    for (const device of state.devices) {
        const bucket = state.connectionsBySource.get(device.key)
        assert.equal(device.number, bucket.length, 'button count is the filter result length')
        grouped.push(...bucket)
        for (const value of bucket) assert.equal(value.sourceKey, device.key)
        assert.deepEqual(bucket, state.connections.filter(value => value.sourceKey === device.key), 'bucket preserves collection order and object identity')
    }
    assert.equal(grouped.length, state.connections.length, 'sources are exhaustive')
    assert.equal(new Set(grouped).size, state.connections.length, 'sources are disjoint')
}

test('DNS connection presentation uses the final node without changing raw routing data', () => {
    const original = connection('dns')
    original.dns = true
    original.chains = Object.freeze(['leaf-node', 'inner-group', 'outer-group'])
    original.rule = 'DomainSuffix'
    original.rulePayload = 'example.test'
    original.uploadSpeed = 0; original.downloadSpeed = 0
    const before = JSON.stringify(original)
    Object.freeze(original.metadata); Object.freeze(original)
    const result = formatConnection(original)
    assert.equal(result.type, 'DNS')
    assert.equal(result.chains, 'leaf-node')
    assert.equal(result.rule, '')
    assert.equal(result.original, original)
    assert.equal(JSON.stringify(original), before)
    assert.equal(formatConnection(original), result, 'unchanged records reuse presentation')
    assert.deepEqual(getConnectionPresentation({ ...original, chains: ['DIRECT'] }), { type: 'DNS', chains: 'DIRECT', rule: '' })
    assert.deepEqual(getConnectionPresentation({ ...original, chains: [] }), { type: 'DNS', chains: '', rule: '' })
})

test('only an explicit boolean DNS flag changes presentation, regardless of type, name, or port', () => {
    const original = connection('ordinary-dns-port')
    original.chains = ['leaf-node', 'inner-group', 'outer-group']
    original.rule = 'DomainSuffix'; original.rulePayload = 'example.test'
    original.metadata.destinationPort = '53'
    original.metadata.inboundName = 'DNS'
    for (const type of ['Socks5', 'HTTP', 'Tun', 'Inner', 'DNS']) {
        const value = { ...original, metadata: { ...original.metadata, type } }
        assert.equal(isDNSConnection(value), false)
        assert.deepEqual(getConnectionPresentation(value), {
            type, chains: 'outer-group / inner-group / leaf-node', rule: 'DomainSuffix :: example.test',
        })
        assert.equal(isDNSConnection({ ...value, dns: true }), true, 'explicit DNS work can retain its original inbound type')
    }
    for (const inboundName of ['DNS', 'DNS-TRANSPORT']) {
        const value = { ...original, metadata: { ...original.metadata, type: 'Inner', inboundName, destinationPort: '443' } }
        assert.equal(isDNSConnection(value), false, 'legacy names are not evidence of a real resolver socket')
        assert.equal(getConnectionPresentation(value).type, 'Inner')
    }
    for (const inboundName of [undefined, '', 'DNS-other', 'dns', 'HTTPS']) {
        assert.equal(isDNSConnection({ ...original, metadata: { ...original.metadata, type: 'Inner', inboundName } }), false)
    }
    assert.equal(isDNSConnection({}), false)
    for (const dns of [false, 'true', 1, null]) assert.equal(isDNSConnection({ ...original, dns }), false)
    assert.deepEqual(getConnectionPresentation({}), { type: '', chains: '', rule: '' })
})

test('source classification separates real resolver sockets, logical clients, internal work, and unknown sources', () => {
    const logical = connection('logical')
    logical.metadata.type = 'Inner'; logical.metadata.inboundName = 'DNS'
    const actualDNS = { ...logical, id: 'actual-dns', dns: true }
    const ordinary53 = connection('ordinary53')
    ordinary53.metadata.destinationPort = '53'; ordinary53.metadata.inboundName = 'DNS'
    const internal = connection('internal', '')
    internal.metadata.type = 'Inner'
    const unknown = connection('unknown', '')
    const store = new ConnectionsStore()
    store.feed([logical, actualDNS, ordinary53, internal, unknown])
    assertCounts(store, { '192.0.2.1': 2, internal: 2, unknown: 1 })
    assert.deepEqual(store.getSnapshot().connectionsBySource.get(INTERNAL_CONNECTIONS).map(value => value.id), ['actual-dns', 'internal'])
    assert.deepEqual(store.getSnapshot().connectionsBySource.get('device:192.0.2.1').map(value => value.id), ['logical', 'ordinary53'])
    assert.equal(actualDNS.metadata.sourceIP, '192.0.2.1', 'classification preserves original tracking metadata')

    for (const sourceIP of [undefined, null, '', '0.0.0.0', '::', '0:0:0:0:0:0:0:0', '::0.0.0.0', '::ffff:0.0.0.0', 'all', 'internal', 'unknown', 'not-an-ip', '256.1.1.1']) {
        for (const type of ['Socks5', 'Inner']) {
            const value = { ...logical, metadata: { ...logical.metadata, sourceIP, type } }
            assert.equal(getConnectionSourceKey(value), type === 'Inner' ? INTERNAL_CONNECTIONS : UNKNOWN_CONNECTIONS, String(sourceIP))
            assert.equal(getConnectionSourceKey({ ...value, dns: true }), INTERNAL_CONNECTIONS)
        }
    }
    assert.equal(getConnectionSourceKey({}), UNKNOWN_CONNECTIONS)
    assert.equal(getConnectionSourceKey(connection('loopback', '127.0.0.1')), 'device:127.0.0.1', 'loopback alone is not evidence of internal work')
    assert.equal(getConnectionSourceKey(connection('v6', '2001:0DB8:0000:0000:0000:0000:0000:0001')), 'device:2001:db8::1')
    assert.equal(getConnectionSourceKey(connection('v6-zone', 'fe80::1%eth0')), 'device:fe80::1%eth0')
})

test('source changes, returning IDs, and history eviction use the same source index', () => {
    const store = new ConnectionsStore(2)
    store.toggleSave()
    const a = connection('A', '')
    const b = connection('B', '::')
    b.metadata.type = 'Inner'
    store.feed([a, b], 1000)
    assertCounts(store, { unknown: 1, internal: 1 })
    store.feed([{ ...a, dns: true }, connection('C')], 2000)
    assertCounts(store, { internal: 2, '192.0.2.1': 1 })
    assert.equal(store.getSnapshot().connectionsBySource.has(UNKNOWN_CONNECTIONS), false)
    store.feed([connection('D')], 3000)
    assertCounts(store, { internal: 1, '192.0.2.1': 2 })
    assert.equal(store.getConnection('B'), undefined, 'oldest internal closed record is evicted')
    store.feed([connection('A', '192.0.2.2')], 4000)
    assertCounts(store, { '192.0.2.1': 2, '192.0.2.2': 1 })
    assert.equal(store.getSnapshot().connectionsBySource.has(INTERNAL_CONNECTIONS), false)
    store.clearHistory()
    assertCounts(store, { '192.0.2.2': 1 })
})

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
