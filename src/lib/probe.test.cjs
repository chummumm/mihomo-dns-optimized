const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { test } = require('node:test')
const ts = require('typescript')

const compiled = ts.transpileModule(fs.readFileSync(path.join(__dirname, 'probe.ts'), 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
}).outputText
const source = { exports: {} }
new Function('module', 'exports', compiled)(source, source.exports)
const { DEFAULT_PROBE_URL, normalizeProbeURL, supportsProxyDelay } = source.exports

test('speed test URL supports HTTP(S), defaults safely, and rejects unusable or credential-bearing input', () => {
    for (const input of [undefined, '', '  ']) assert.equal(normalizeProbeURL(input), DEFAULT_PROBE_URL)
    assert.equal(normalizeProbeURL(' https://example.test/generate_204?check=1 '), 'https://example.test/generate_204?check=1')
    assert.equal(normalizeProbeURL('http://[2001:db8::1]:8080/204'), 'http://[2001:db8::1]:8080/204')
    for (const input of ['example.test', '/204', 'ftp://example.test', 'javascript:alert(1)', 'https://user:secret@example.test/204', 'https://example.test/204#fragment', 'https://example.test/' + 'x'.repeat(2048), 1, null, {}]) {
        assert.equal(normalizeProbeURL(input), undefined, String(input).slice(0, 80))
    }
})

test('built-in policies never initiate delay tests while ordinary leaf proxies remain testable', () => {
    for (const name of ['DIRECT', 'REJECT', 'REJECT-DROP', 'PASS']) assert.equal(supportsProxyDelay({ name, type: 'Socks' }), false)
    for (const type of ['Direct', 'Reject', 'RejectDrop', 'Pass']) assert.equal(supportsProxyDelay({ name: 'custom-name', type }), false)
    for (const type of ['Socks', 'Http', 'Shadowsocks', 'Trojan', 'Vmess']) assert.equal(supportsProxyDelay({ name: 'node', type }), true)
})

test('controller protocol overrides preserve HTTPS and accept only HTTP(S)', () => {
    const output = ts.transpileModule(fs.readFileSync(path.join(__dirname, 'controller.ts'), 'utf8'), {
        compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
    }).outputText
    const source = { exports: {} }
    new Function('module', 'exports', output)(source, source.exports)
    const { controllerProtocol } = source.exports
    assert.equal(controllerProtocol('https:', '127.0.0.1', undefined, 'http:'), 'https:', 'explicit HTTPS is not downgraded by conditional precedence')
    assert.equal(controllerProtocol('http:', 'example.test', 'https:', 'https:'), 'http:')
    assert.equal(controllerProtocol(null, '127.0.0.1', undefined, 'https:'), 'http:')
    assert.equal(controllerProtocol(null, 'example.test', 'https:', 'http:'), 'https:')
    for (const override of ['', 'ftp:', 'javascript:', 'file:', 'https']) {
        assert.equal(controllerProtocol(override, 'example.test', undefined, 'https:'), 'https:')
    }
})
