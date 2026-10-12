const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { test } = require('node:test')
const ts = require('typescript')

const compiled = ts.transpileModule(fs.readFileSync(path.join(__dirname, 'pagination.ts'), 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
}).outputText
const source = { exports: {} }
new Function('module', 'exports', compiled)(source, source.exports)
const { readPageIndex, cursorForPage, pageCount } = source.exports

const response = { retained: 4096, total: 120, anchor_id: '18446744073709551615', page_cursors: ['', '18446744073709551111', '18446744073709550555'] }

test('filtered page boundaries preserve uint64 IDs and exact matched totals', () => {
    const index = readPageIndex(response, 50)
    assert.equal(index.total, 120, 'the filtered count is independent of global retained count')
    assert.equal(index.anchor, '18446744073709551615')
    assert.equal(pageCount(index.total, 50), 3)
    assert.equal(cursorForPage(index, 1), '')
    assert.equal(cursorForPage(index, 2), '18446744073709551111', 'a sparse boundary must not use numeric ID subtraction')
    assert.equal(cursorForPage(index, 3), '18446744073709550555')
    for (const target of [0, -1, 1.5, 4, Number.MAX_SAFE_INTEGER + 1, NaN, Infinity]) assert.equal(cursorForPage(index, target), undefined)
})

test('older cores leave totals unknown and empty retained histories have one navigable page', () => {
    assert.equal(readPageIndex({ retained: 4096, items: Array(50), has_more: true }, 50), undefined)
    const empty = readPageIndex({ retained: 150, total: 0, anchor_id: '150', page_cursors: [''] }, 50)
    assert.equal(empty.total, 0)
    assert.equal(pageCount(empty.total, 50), 1)
    assert.equal(cursorForPage(empty, 2), undefined)
    assert.equal(readPageIndex({ retained: 0, total: 0, anchor_id: '', page_cursors: [''] }, 50).anchor, '')
})

test('evicted tails replace the complete page index without retaining stale boundaries', () => {
    const previous = readPageIndex(response, 50)
    const next = readPageIndex({ ...response, total: 72, page_cursors: response.page_cursors.slice(0, 2) }, 50)
    assert.equal(previous.cursors.length, 3)
    assert.equal(next.total, 72)
    assert.equal(next.anchor, previous.anchor)
    assert.equal(cursorForPage(next, 3), undefined, 'an expired last page cannot remain selectable')
})

test('malformed or inconsistent metadata cannot enable exact totals or direct jumps', () => {
    for (const invalid of [
        { total: -1 }, { total: 4097 }, { total: 120.5 }, { total: NaN }, { total: '120' },
        { anchor_id: '' }, { anchor_id: '0' }, { anchor_id: '18446744073709551616' },
        { page_cursors: null }, { page_cursors: [] }, { page_cursors: ['', '123'] },
        { page_cursors: ['123', '122', '121'] }, { page_cursors: ['', '123', '123'] },
        { page_cursors: ['', '123', '124'] }, { page_cursors: ['', '0', '1'] },
        { anchor_id: '100', page_cursors: ['', '120', '60'] },
    ]) assert.equal(readPageIndex({ ...response, ...invalid }, 50), undefined, JSON.stringify(invalid))
})
