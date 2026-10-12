const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { test } = require('node:test')
const ts = require('typescript')

function load (file) {
    const compiled = ts.transpileModule(fs.readFileSync(path.join(__dirname, file), 'utf8'), {
        compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
    }).outputText
    const source = { exports: {} }
    new Function('module', 'exports', compiled)(source, source.exports)
    return source.exports
}

const { buildTrend, niceTicks, timeTicks, monotonePath } = load('trend.ts')

const MINUTE = 60000
const end = Date.UTC(2030, 0, 1, 12, 0)
const series = Array.from({ length: 1440 }, (_, index) => ({
    time: new Date(end - (1439 - index) * MINUTE).toISOString(),
    queries: index === 1439 ? 999 : index % 10,
    cache_hits: index % 5,
    errors: index === 1439 ? 99 : 0,
    elapsed_ms_avg: 1,
}))

test('the still-filling kernel minute is never drawn', () => {
    const points = buildTrend(series, 1, 1)
    assert.equal(points.length, 60)
    assert.equal(points.at(-1).start, end - MINUTE)
    assert(points.every(point => point.queries < 999 && point.errors === 0))
})

test('longer ranges average per minute over clock-aligned buckets', () => {
    const points = buildTrend(series, 24, 10)
    assert(points.length >= 144 && points.length <= 145)
    const full = points.filter(point => point.minutes === 10)
    assert(full.length >= 143)
    for (const point of full) {
        assert.equal(point.queries, 4.5, 'mean of 0..9 per minute')
        assert.equal(point.totalQueries, 45)
        assert.equal(new Date(point.start).getMinutes() % 10, 0, 'buckets start on the local clock')
    }
    assert.deepEqual(buildTrend([], 24, 10), [])
    assert.equal(buildTrend(series.slice(0, 1), 1, 1).length, 1, 'a single bucket is still shown')
})

test('axis helpers produce round ticks and clock times', () => {
    assert.deepEqual(niceTicks(0), [0, 1])
    assert.deepEqual(niceTicks(87), [0, 25, 50, 75, 100])
    assert.deepEqual(niceTicks(0.3), [0, 0.1, 0.2, 0.3])
    const ticks = timeTicks(end - 24 * 60 * MINUTE, end, 900)
    assert(ticks.length >= 4 && ticks.length <= 14)
    assert(ticks.every(time => time >= end - 24 * 60 * MINUTE && time <= end))
    assert(ticks.every(time => new Date(time).getMinutes() % 5 === 0))
    assert.deepEqual(timeTicks(end, end, 900), [])
    assert.match(monotonePath([[0, 10], [10, 0], [20, 10]]), /^M0\.0,10\.0 C/)
})

