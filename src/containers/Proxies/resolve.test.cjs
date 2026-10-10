const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { test } = require('node:test')
const ts = require('typescript')

const compiled = ts.transpileModule(fs.readFileSync(path.join(__dirname, 'resolve.ts'), 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
}).outputText
const source = { exports: {} }
new Function('module', 'exports', compiled)(source, source.exports)
const { resolveProxyExit } = source.exports

test('nested group presentation reaches the real leaf beyond eight hops, including GLOBAL', () => {
    const groups = new Map([['GLOBAL', { now: 'group-0' }]])
    for (let i = 0; i < 20; i++) groups.set(`group-${i}`, { now: i === 19 ? 'REJECT-Tokyo' : `group-${i + 1}` })
    assert.equal(resolveProxyExit('GLOBAL', groups), 'REJECT-Tokyo')
    assert.equal(resolveProxyExit('DIRECT', groups), 'DIRECT')
    assert.equal(resolveProxyExit('node-a', groups), 'node-a')
    assert.equal(groups.get('GLOBAL').now, 'group-0', 'presentation does not change selected routes')
})

test('empty and cyclic selections have no invented final exit', () => {
    const groups = new Map([
        ['empty', { now: '' }], ['unset', {}],
        ['self', { now: 'self' }],
        ['first', { now: 'second' }], ['second', { now: 'first' }],
    ])
    for (const name of ['', 'empty', 'unset', 'self', 'first', 'second']) {
        assert.equal(resolveProxyExit(name, groups), undefined, name)
    }
})
