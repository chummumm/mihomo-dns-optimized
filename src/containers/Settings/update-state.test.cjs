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
const { readUpdateSnapshot, reconcileUpdateRestart, updateTaskActive } = source.exports

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
