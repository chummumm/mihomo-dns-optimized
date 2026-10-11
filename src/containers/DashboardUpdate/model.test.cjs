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
const { readDashboardUpdateStatus, compareDashboardVersions, canInstallDashboardUpdate, isCoreHostedDashboard, dashboardControllerOrigin } = source.exports

test('unversioned sources remain available for explicit manual installation', () => {
    const generic = { supported: true, update_available: false, updating: false, reason: 'version-unavailable', current_version: '', current_commit: '', current_digest: 'a'.repeat(64), latest_digest: 'b'.repeat(64) }
    assert.deepEqual(readDashboardUpdateStatus(generic), generic)
    const unversioned = { supported: true, update_available: true, updating: false, current_version: '', current_commit: '' }
    assert.deepEqual(readDashboardUpdateStatus(unversioned), unversioned, 'older cores remain compatible when scheduler and version-comparison metadata is absent')
    const thirdParty = { ...generic, current_version: 'v1.2.3-beta.4', latest_version: '2026-10-11', latest_commit: 'build-abc' }
    assert.deepEqual(readDashboardUpdateStatus(thirdParty), thirdParty, 'unrecognized source versions can still be displayed and installed manually')
})

test('malformed status and digest identity data cannot enable updater operations', () => {
    const valid = { supported: true, update_available: false, updating: false, current_version: '', current_commit: '' }
    for (const response of [null, '<html>old core</html>', {}, { ...valid, supported: 'true' }, { ...valid, updating: 0 }, { ...valid, current_version: 'x'.repeat(65) }, { ...valid, check_error: {} }, { ...valid, reason: 'x'.repeat(513) }, { ...valid, current_digest: 'abc' }, { ...valid, latest_digest: 1 }, { ...valid, latest_digest: 'g'.repeat(64) }]) {
        assert.equal(readDashboardUpdateStatus(response), null)
    }
    assert.deepEqual(readDashboardUpdateStatus({ ...valid, supported: false, reason: 'ui-not-installed' }), { ...valid, supported: false, reason: 'ui-not-installed' })
})

test('dashboard versions follow strict SemVer precedence without numeric precision loss', () => {
    assert.equal(compareDashboardVersions('0.3.10', '0.3.9'), 1)
    assert.equal(compareDashboardVersions('1.0.0', '0.99.99'), 1)
    assert.equal(compareDashboardVersions('0.3.5', '0.3.5'), 0)
    assert.equal(compareDashboardVersions('0.2.9', '0.3.0'), -1)
    assert.equal(compareDashboardVersions('v0.3.7+new-build.2', '0.3.7+old-build.1'), 0)
    assert.equal(compareDashboardVersions('9007199254740993.0.0', '9007199254740992.0.0'), 1)
    assert.equal(compareDashboardVersions('0.3.7-9007199254740993', '0.3.7-9007199254740992'), 1)
    const precedence = ['1.0.0-alpha', '1.0.0-alpha.1', '1.0.0-alpha.beta', '1.0.0-beta', '1.0.0-beta.2', '1.0.0-beta.11', '1.0.0-rc.1', '1.0.0']
    for (let i = 1; i < precedence.length; i++) {
        assert.equal(compareDashboardVersions(precedence[i - 1], precedence[i]), -1)
        assert.equal(compareDashboardVersions(precedence[i], precedence[i - 1]), 1)
    }
    for (const version of ['', 'v1', 'v1.2', 'V1.2.3', 'vv1.2.3', '0.3', '1.2.3.4', '01.2.3', '1.02.3', '1.2.03', '1.2.3-01', '1.2.3-alpha..1', '1.2.3+', '1.2.3+build_1', '1.2.3\n', ' 1.2.3', '1.2.3 ', '1.2.3-' + 'a'.repeat(59)]) {
        assert.equal(compareDashboardVersions(version, '0.3.7'), null, version)
    }
})

test('manual installation is offered only for newer or explicitly unversioned sources', () => {
    const status = { supported: true, update_available: false, updating: false, current_version: '0.3.7', current_commit: '', current_digest: 'a'.repeat(64), latest_digest: 'b'.repeat(64) }
    assert.equal(canInstallDashboardUpdate(null), false)
    assert.equal(canInstallDashboardUpdate({ ...status, latest_version: '0.3.7' }), false, 'changed content at the same version does not expose installation')
    assert.equal(canInstallDashboardUpdate({ ...status, latest_version: '0.3.6' }), false, 'older versions do not expose installation')
    assert.equal(canInstallDashboardUpdate({ ...status, latest_version: '0.3.8', update_available: true }), true)
    const manual = { ...status, latest_version: '', reason: 'version-unavailable' }
    assert.equal(canInstallDashboardUpdate(manual), true)
    assert.equal(canInstallDashboardUpdate({ ...manual, supported: false }), false)
    assert.equal(canInstallDashboardUpdate({ ...manual, check_error: 'download-failed' }), false)
    assert.equal(canInstallDashboardUpdate({ ...manual, updating: true }), false)
})

test('core scheduler metadata is optional and validated before displaying it', () => {
    const status = { supported: true, update_available: false, updating: false, current_version: '', current_commit: '', auto_update: true, update_interval: 8, next_check: '2026-10-12T16:00:00Z' }
    assert.deepEqual(readDashboardUpdateStatus(status), status)
    for (const invalid of [{ auto_update: 'true' }, { update_interval: 0 }, { update_interval: -1 }, { update_interval: 1.5 }, { update_interval: '24' }, { update_interval: Infinity }, { update_interval: Number.MAX_SAFE_INTEGER + 1 }, { next_check: 'not a date' }, { next_check: 0 }]) {
        assert.equal(readDashboardUpdateStatus({ ...status, ...invalid }), null)
    }
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
