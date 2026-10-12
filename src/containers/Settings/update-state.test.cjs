const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { test } = require('node:test')
const ts = require('typescript')

const compiled = ts.transpileModule(fs.readFileSync(path.join(__dirname, 'update-state.ts'), 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
}).outputText
const source = { exports: {} }
new Function('module', 'exports', compiled)(source, source.exports)
const { currentUpdateSchedules, formatUpdateTime, nextUpdateTime, readUpdateSnapshot, reconcileUpdateRestart, updateTaskActive } = source.exports

function task (instance = 'old', current = 'mihomo-optimized-v1.0.3', latest = 'mihomo-optimized-v1.0.4') {
    return { instance_id: instance, task_id: 'accepted-task', state: 'restarting', started_at: '2026-10-12T00:00:00Z', items: [
        { id: 'kernel', kind: 'kernel', name: 'mihomo-optimized', status: 'restarting', current_version: current, latest_version: latest },
        { id: 'ui', kind: 'ui', name: 'mihomo', status: 'updated', current_version: '0.4.6', auto_update: false, update_interval: 24, interval_unit: 'hours' },
        { id: 'rule-provider:rules', kind: 'rule-provider', name: 'rules', status: 'up_to_date', auto_update: true, update_interval: 3600, interval_unit: 'seconds' },
    ] }
}

test('tasks preserve controller policy units and partial results without client version comparison', () => {
    const value = task()
    assert.deepEqual(readUpdateSnapshot(value), value)
    assert.equal(updateTaskActive(value), true)
    value.state = 'partial'
    value.items[2].status = 'error'
    value.items[2].error = 'update_failed'
    assert.equal(updateTaskActive(readUpdateSnapshot(value)), false)
    assert.equal(readUpdateSnapshot(value).items[2].status, 'error')
})

test('malformed controller data cannot enter the update view', () => {
    for (const value of [null, [], {}, { ...task(), instance_id: '' }, { ...task(), task_id: undefined }, { ...task(), request_id: '' }, { ...task(), request_id: {} }, { ...task(), state: 'available' }, { ...task(), items: null }, { ...task(), finished_at: { toString: 'bad' } }, { ...task(), started_at: 'yesterday' }]) {
        assert.equal(readUpdateSnapshot(value), undefined)
    }
    for (const patch of [{ kind: 'unknown' }, { status: 'installing' }, { latest_version: {} }, { auto_update: 'true' }, { interval_unit: 'days' }, { update_interval: NaN }]) {
        const value = task()
        value.items[0] = { ...value.items[0], ...patch }
        assert.equal(readUpdateSnapshot(value), undefined)
    }
    const duplicate = task()
    duplicate.items.push({ ...duplicate.items[0] })
    assert.equal(readUpdateSnapshot(duplicate), undefined)
})

test('core success after reconnect requires a new process running the exact requested version', () => {
    const previous = task()
    const current = task('new', 'mihomo-optimized-v1.0.4')
    current.state = 'idle'
    assert.equal(reconcileUpdateRestart(previous, current), 'verified')
    assert.equal(reconcileUpdateRestart(previous, task('old', 'mihomo-optimized-v1.0.4')), undefined)
    assert.equal(reconcileUpdateRestart(undefined, current), undefined)
})

test('unrelated restart or an unknown target never claims the upgrade succeeded', () => {
    assert.equal(reconcileUpdateRestart(task(), task('new', 'mihomo-optimized-v1.0.3')), 'unconfirmed')
    assert.equal(reconcileUpdateRestart(task(), task('new', 'mihomo-optimized-v1.0.5')), 'unconfirmed')
    assert.equal(reconcileUpdateRestart(task('old', 'mihomo-optimized-v1.0.4'), task('new', 'mihomo-optimized-v1.0.4')), 'unconfirmed')
    const noTarget = task()
    delete noTarget.items[0].latest_version
    assert.equal(reconcileUpdateRestart(noTarget, task('new', 'mihomo-optimized-v1.0.4')), 'unconfirmed')
    const completed = task()
    completed.state = 'completed'
    assert.equal(reconcileUpdateRestart(completed, task('new', 'mihomo-optimized-v1.0.4')), undefined)
})

test('automatic schedules validate all four kinds and preserve old controller compatibility', () => {
    for (const kind of ['kernel', 'ui', 'geodata', 'rule-provider']) {
        const value = { instance_id: 'scheduled', state: 'idle', items: [{ id: kind, kind, name: kind, status: 'pending', auto_update: true, next_check: '1970-01-02T00:00:00Z', next_check_in_ms: 86400000, auto_update_state: 'scheduled' }] }
        assert.deepEqual(readUpdateSnapshot(value), value)
        for (const patch of [{ next_check: 'tomorrow' }, { next_check: 0 }, { next_check_in_ms: NaN }, { next_check_in_ms: -1 }, { next_check_in_ms: 0.5 }, { next_check_in_ms: Number.MAX_SAFE_INTEGER + 1 }, { auto_update_state: 'invented' }]) {
            assert.equal(readUpdateSnapshot({ ...value, items: [{ ...value.items[0], ...patch }] }), undefined)
        }
    }
    assert(readUpdateSnapshot(task()), 'old controller snapshots remain valid without scheduling fields')
})

test('next check follows browser wall clock and real remaining time without delaying slow responses', () => {
    const browserNow = Date.parse('2026-10-12T16:30:00Z')
    const item = { auto_update: true, next_check: '1970-01-02T00:00:00Z', next_check_in_ms: 3600000 }
    const received = nextUpdateTime(item, 100, browserNow, 2100)
    assert.equal(received, browserNow + 3600000 - 2000, 'device clock is ignored and request round trip is deducted exactly once')
    assert.equal(nextUpdateTime(item, 100, browserNow + 60000, 62100), received, 'aging a cached status never restarts its countdown')
    assert.equal(nextUpdateTime(item, 100, browserNow + 7200000, 2100), received + 7200000, 'correcting the browser wall clock changes the displayed calendar time')
    assert.equal(nextUpdateTime({ ...item, next_check_in_ms: 0 }, 100, browserNow, 2100), browserNow - 2000, 'due time stays due')
    assert.equal(nextUpdateTime({ auto_update: true, update_interval: 24 }, 100, browserNow, 2100), undefined, 'an interval alone cannot invent a next check')
    assert.equal(nextUpdateTime({ ...item, auto_update: false }, 100, browserNow, 2100), undefined)
    assert.equal(nextUpdateTime({ ...item, reason: 'local_provider' }, 100, browserNow, 2100), undefined)
    assert.equal(nextUpdateTime({ ...item, reason: 'local_provider', auto_update_state: 'scheduled' }, 100, browserNow, 2100), received, 'new remote policy is not hidden by a retained historical local-provider result')
    assert.equal(nextUpdateTime({ ...item, auto_update_state: 'unscheduled' }, 100, browserNow, 2100), undefined)
    assert.equal(nextUpdateTime({ ...item, next_check_in_ms: Number.MAX_SAFE_INTEGER }, 100, browserNow, 2100), undefined, 'an unrepresentable calendar date never crashes rendering')
    assert.equal(nextUpdateTime({ auto_update: true, next_check: '2026-10-13T00:00:00Z' }, undefined, browserNow, 2100), Date.parse('2026-10-13T00:00:00Z'))
})

test('formatting uses the browser default time zone and includes the calendar date', () => {
    const instant = Date.parse('2026-10-12T23:30:00Z')
    const original = process.env.TZ
    try {
        for (const zone of ['Asia/Shanghai', 'America/Los_Angeles']) {
            process.env.TZ = zone
            const expected = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', timeZoneName: 'short' }).format(instant)
            assert.equal(formatUpdateTime(instant), expected)
        }
    } finally {
        if (original === undefined) delete process.env.TZ
        else process.env.TZ = original
    }
})

test('retained manual results use the new process schedule and clear removed provider schedules', () => {
    const previous = [
        { id: 'kernel', status: 'updated', auto_update: true, next_check_in_ms: 86400000, auto_update_state: 'scheduled' },
        { id: 'rule-provider:old', status: 'updated', auto_update: true, next_check_in_ms: 3600000 },
    ]
    const current = [{ id: 'kernel', auto_update: false }]
    const items = currentUpdateSchedules(previous, current)
    assert.equal(items[0].status, 'updated')
    assert.equal(items[0].auto_update, false)
    assert.equal(items[0].next_check_in_ms, undefined)
    assert.equal(items[1].auto_update, undefined)
    assert.equal(items[1].next_check_in_ms, undefined)
    assert.equal(previous[0].next_check_in_ms, 86400000, 'history remains immutable')
})
