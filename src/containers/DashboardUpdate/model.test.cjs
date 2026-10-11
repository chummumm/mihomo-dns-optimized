const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { test } = require('node:test')
const ts = require('typescript')

const compiled = ts.transpileModule(fs.readFileSync(path.join(__dirname, 'model.ts'), 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
}).outputText
const source = { exports: {} }
new Function('module', 'exports', compiled)(source, source.exports)
const { readDashboardUpdateStatus, compareDashboardVersions, isCoreHostedDashboard, dashboardControllerOrigin } = source.exports

test('configured generic dashboard updates work without build-info or semver metadata', () => {
    const generic = { supported: true, update_available: true, updating: false, current_version: '', current_commit: '', current_digest: 'a'.repeat(64), latest_digest: 'b'.repeat(64) }
    assert.deepEqual(readDashboardUpdateStatus(generic), generic)
    const unversioned = { supported: true, update_available: true, updating: false, current_version: '', current_commit: '' }
    assert.deepEqual(readDashboardUpdateStatus(unversioned), unversioned, 'the core decides update availability even when no build metadata exists')
    const thirdParty = { ...generic, current_version: 'v1.2.3-beta.4', latest_version: '2026-10-11', latest_commit: 'build-abc' }
    assert.deepEqual(readDashboardUpdateStatus(thirdParty), thirdParty, 'foreign version naming is not a reason to reject the configured UI')
})

test('malformed status and digest identity data cannot enable updater operations', () => {
    const valid = { supported: true, update_available: false, updating: false, current_version: '', current_commit: '' }
    for (const response of [null, '<html>old core</html>', {}, { ...valid, supported: 'true' }, { ...valid, updating: 0 }, { ...valid, current_version: 'x'.repeat(65) }, { ...valid, check_error: {} }, { ...valid, reason: 'x'.repeat(513) }, { ...valid, current_digest: 'abc' }, { ...valid, latest_digest: 1 }, { ...valid, latest_digest: 'g'.repeat(64) }]) {
        assert.equal(readDashboardUpdateStatus(response), null)
    }
    assert.deepEqual(readDashboardUpdateStatus({ ...valid, supported: false, reason: 'ui-not-installed' }), { ...valid, supported: false, reason: 'ui-not-installed' })
})

test('dashboard local-version comparison is numeric and does not interpret unknown formats', () => {
    assert.equal(compareDashboardVersions('0.3.10', '0.3.9'), 1)
    assert.equal(compareDashboardVersions('1.0.0', '0.99.99'), 1)
    assert.equal(compareDashboardVersions('0.3.5', '0.3.5'), 0)
    assert.equal(compareDashboardVersions('0.2.9', '0.3.0'), -1)
    for (const version of ['', 'v0.3.6', '1.2.3-beta', '0.3', '1.2.3.4', '9999999.0.0']) assert.equal(compareDashboardVersions(version, '0.3.5'), null)
})

test('invalid saved controllers do not crash the app and cannot become a dashboard update link', () => {
    assert.equal(dashboardControllerOrigin('http:', '127.0.0.1', 9090), 'http://127.0.0.1:9090')
    assert.equal(dashboardControllerOrigin('https:', 'example.test', 443), 'https://example.test')
    assert.equal(dashboardControllerOrigin('http:', '::1', 9090), 'http://[::1]:9090')
    assert.equal(dashboardControllerOrigin('https:', '[2001:db8::1]', 443), 'https://[2001:db8::1]')
    for (const input of [['ftp:', 'host', 90], ['http:', '127.0.0.1', 'abc'], ['http:', 'host', 65536], ['http:', '', 90], ['http:', 'user@host', 90], ['http:', 'host/path', 90], ['http:', 'host?query', 90], ['http:', '[bad-ip]', 90]]) {
        assert.equal(dashboardControllerOrigin(...input), null)
    }
    assert.equal(isCoreHostedDashboard({ origin: 'http://127.0.0.1:9090', pathname: '/ui/' }, 'http://127.0.0.1:9090'), true)
    assert.equal(isCoreHostedDashboard({ origin: 'http://127.0.0.1:9090', pathname: '/ui-alt/' }, 'http://127.0.0.1:9090'), false)
    assert.equal(isCoreHostedDashboard({ origin: 'https://example.test', pathname: '/ui/' }, 'http://127.0.0.1:9090'), false)
})
