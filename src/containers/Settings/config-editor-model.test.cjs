const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { test } = require('node:test')
const ts = require('typescript')

const compiled = ts.transpileModule(fs.readFileSync(path.join(__dirname, 'config-editor-model.ts'), 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
}).outputText
const source = { exports: {} }
new Function('module', 'exports', compiled)(source, source.exports)
const { MAX_CONFIG_FILE_BYTES, configFits, readConfigFile, configLineStarts, configCursor, configDisplayText, configSaveText } = source.exports

test('file cap counts UTF-8 bytes, not JavaScript code units, without splitting surrogate pairs', () => {
    for (const text of ['', 'port: 7890\n', '代理: 直连\n', '🐟', '\ud800', '\udc00', '\ud800a', 'a\udfff']) {
        const bytes = Buffer.byteLength(text)
        assert.equal(configFits(text, bytes), true)
        if (bytes > 0) assert.equal(configFits(text, bytes - 1), false)
    }
    assert.equal(configFits('a'.repeat(MAX_CONFIG_FILE_BYTES), MAX_CONFIG_FILE_BYTES), true)
    assert.equal(configFits('a'.repeat(MAX_CONFIG_FILE_BYTES + 1), MAX_CONFIG_FILE_BYTES), false)
})

test('old, malformed and oversized endpoint responses cannot enable saving', () => {
    const valid = { path: '/fixture/config.yaml', revision: 'revision-1', content: 'mode: rule\n', max_size: MAX_CONFIG_FILE_BYTES }
    assert.deepEqual(readConfigFile(valid), valid)
    for (const response of [null, '', '<html>old kernel</html>', {}, { ...valid, revision: '' }, { ...valid, max_size: 0 }, { ...valid, max_size: 1.1 }, { ...valid, content: 1 }, { ...valid, content: 'a'.repeat(MAX_CONFIG_FILE_BYTES + 1) }]) {
        assert.equal(readConfigFile(response), undefined)
    }
    assert.equal(readConfigFile({ ...valid, max_size: MAX_CONFIG_FILE_BYTES * 4 }).max_size, MAX_CONFIG_FILE_BYTES)
    assert.equal(readConfigFile({ ...valid, max_size: 4 }), undefined)
})

test('cursor lookup matches text boundaries and uses a bounded index even for newline-heavy files', () => {
    for (const text of ['', 'abc', '\n', '\n\n', '代理\nmode: rule\n\nlast']) {
        const starts = configLineStarts(text)
        for (let offset = 0; offset <= text.length; offset++) {
            const before = text.slice(0, offset).split('\n')
            assert.deepEqual(configCursor(starts, offset), { line: before.length, column: before.at(-1).length + 1 })
        }
    }
    const text = '\n'.repeat(100000)
    const starts = configLineStarts(text)
    assert.equal(starts.byteLength, (text.length + 1) * 4)
    assert.deepEqual(configCursor(starts, 99999), { line: 100000, column: 1 })
})

test('CRLF file is clean on open, has correct textarea offsets, and preserves its line endings on save', () => {
    const original = 'mode: rule\r\nport: 7890\r\n'
    const display = configDisplayText(original)
    assert.equal(display, 'mode: rule\nport: 7890\n')
    assert.deepEqual(configCursor(configLineStarts(display), 11), { line: 2, column: 1 })
    assert.equal(configSaveText(display.replace('7890', '7891'), original), 'mode: rule\r\nport: 7891\r\n')
})
