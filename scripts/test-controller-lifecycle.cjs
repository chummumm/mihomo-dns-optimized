#!/usr/bin/env node
// Controller-owned state and save/reload completion against isolated HTTP fixtures.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const path = require('node:path')
const { chromium } = require('playwright')

const dist = path.resolve(process.env.UI_DIST_DIR || path.join(__dirname, '../dist'))
const controllers = []
const held = new Set()
const pageErrors = []
let browser

function json(res, status, body) {
    res.writeHead(status, { 'content-type': 'application/json', 'cache-control': 'no-store' })
    res.end(JSON.stringify(body))
}

function controller(name) {
    const value = { name, holdState: false, reload: 'normal', holdGeneralAfterReload: false, generalHeld: false, writes: 0, reloads: 0 }
    value.file = { path: '/fixture/config.yaml', content: 'mode: rule\n', revision: 'initial', max_size: 4194304, writable: true }
    value.server = http.createServer(async (req, res) => {
        res.setHeader('Access-Control-Allow-Origin', '*')
        res.setHeader('Access-Control-Allow-Headers', 'Authorization, Content-Type')
        res.setHeader('Access-Control-Allow-Methods', 'GET, PUT, PATCH, OPTIONS')
        if (req.method === 'OPTIONS') { res.writeHead(204); res.end(); return }
        const url = new URL(req.url, 'http://127.0.0.1')
        const leaf = { name: `node-${name}`, type: 'Socks', history: [], udp: true }
        const provider = { name: `provider-${name}`, type: 'Socks', history: [], udp: true }
        const payloads = {
            '/version': { version: `controller-${name}-core`, premium: false },
            '/configs': { port: 7890, 'socks-port': 7891, 'mixed-port': 0, 'redir-port': 0, mode: 'rule', 'log-level': 'info', 'allow-lan': false },
            '/providers/proxies': { providers: { fixture: { name: 'fixture', type: 'Proxy', vehicleType: 'File', proxies: [provider] } } },
            '/proxies': { proxies: {
                GLOBAL: { name: 'GLOBAL', type: 'Selector', now: leaf.name, history: [], all: [leaf.name, 'selected'] },
                selected: { name: 'selected', type: 'Selector', now: leaf.name, history: [], all: [leaf.name] },
                [leaf.name]: leaf,
            } },
            '/providers/rules': { providers: {} },
            '/rules': { rules: [{ type: 'Domain', payload: `rule-${name}.fixture`, proxy: 'DIRECT' }] },
        }
        function hold(body) {
            const item = { controller: value, path: url.pathname, res, body }
            held.add(item)
            res.on('close', () => held.delete(item))
        }
        if (url.pathname === '/configs/file') {
            if (req.method === 'PUT') {
                let body = ''
                for await (const chunk of req) body += chunk
                const data = JSON.parse(body)
                if (data.revision !== value.file.revision) { json(res, 409, {}); return }
                if (data.content !== value.file.content) {
                    value.file = { ...value.file, content: data.content, revision: `saved-${++value.writes}` }
                }
            }
            json(res, 200, value.file); return
        }
        if (url.pathname === '/configs' && req.method === 'PUT') {
            value.reloads++
            if (value.reload === 'hang') { hold({}); return }
            if (value.holdGeneralAfterReload) value.generalHeld = true
            res.writeHead(204); res.end(); return
        }
        if (payloads[url.pathname]) {
            if ((value.holdState && ['/version', '/proxies', '/providers/proxies', '/rules'].includes(url.pathname)) || (url.pathname === '/configs' && value.generalHeld)) {
                hold(payloads[url.pathname]); return
            }
            json(res, 200, payloads[url.pathname]); return
        }
        const filename = path.join(dist, url.pathname === '/' ? 'index.html' : url.pathname)
        if (!filename.startsWith(dist + path.sep) || !fs.existsSync(filename) || !fs.statSync(filename).isFile()) { res.writeHead(404); res.end(); return }
        const types = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml', '.png': 'image/png', '.webmanifest': 'application/manifest+json' }
        res.writeHead(200, { 'content-type': types[path.extname(filename)] || 'application/octet-stream' })
        fs.createReadStream(filename).pipe(res)
    })
    controllers.push(value)
    return value
}

async function until(predicate, message) {
    const deadline = Date.now() + 5000
    while (!await predicate() && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 10))
    assert.ok(await predicate(), message)
}

function release(current, status = 200) {
    for (const item of [...held]) {
        if (item.controller !== current) continue
        held.delete(item)
        json(item.res, item.path === '/version' ? status : 200, item.body)
    }
}

async function pageFor(current, route) {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'en-US', serviceWorkers: 'block' })
    await context.route('**/*', request => {
        const url = new URL(request.request().url())
        return url.hostname === '127.0.0.1' && controllers.some(item => String(item.port) === url.port) ? request.continue() : request.abort()
    })
    await context.routeWebSocket('**/*', socket => {
        if (new URL(socket.url()).pathname === '/connections') socket.send(JSON.stringify({ connections: [], uploadTotal: 0, downloadTotal: 0 }))
    })
    await context.addInitScript(port => {
        localStorage.setItem('language', JSON.stringify('en_US'))
        localStorage.setItem('externalControllers', JSON.stringify([{ hostname: '127.0.0.1', port, secret: '' }]))
        localStorage.setItem('externalControllerIndex', '0')
        // Shorten only an existing reload deadline, keeping an unbounded request
        // unbounded so the old implementation fails this behavioral regression.
        const open = XMLHttpRequest.prototype.open
        const send = XMLHttpRequest.prototype.send
        XMLHttpRequest.prototype.open = function(method, url, ...rest) {
            this.fixtureReload = method.toUpperCase() === 'PUT' && new URL(url, location.href).pathname === '/configs'
            return open.call(this, method, url, ...rest)
        }
        XMLHttpRequest.prototype.send = function(body) {
            if (this.fixtureReload && this.timeout > 0) this.timeout = 150
            return send.call(this, body)
        }
    }, current.port)
    const page = await context.newPage()
    page.setDefaultTimeout(5000)
    page.on('pageerror', error => pageErrors.push(String(error)))
    await page.goto(`http://127.0.0.1:${current.port}/#/${route}`, { waitUntil: 'networkidle' })
    return { page, context }
}

async function controllerIsolation(a, b) {
    const { page, context } = await pageFor(a, 'proxies')
    try {
        const nodes = name => page.locator(`.proxy-item[data-node="node-${name}"]`)
        const providers = name => page.locator(`.proxy-item[data-node="provider-${name}"]`)
        const core = page.locator('.sidebar-version-row').first()
        const switchTo = current => page.evaluate(port => { location.hash = `/proxies?host=127.0.0.1&port=${port}` }, current.port)
        await nodes('a').waitFor()
        await providers('a').waitFor()
        b.holdState = true
        await switchTo(b)
        await until(() => [...held].filter(item => item.controller === b).length >= 3, 'new controller state reads are pending')
        assert.equal(await nodes('a').count(), 0, 'the previous controller nodes disappear before the new response')
        assert.equal(await providers('a').count(), 0, 'the previous controller provider nodes disappear before the new response')
        assert(!((await core.textContent()).includes('controller-a-core')), 'the previous controller version is hidden during the new request')
        b.holdState = false
        release(b)
        await nodes('b').waitFor()
        await providers('b').waitFor()
        await until(async () => (await core.textContent()).includes('controller-b-core'), 'new controller version is displayed')

        for (const oldVersionStatus of [200, 401]) {
            a.holdState = true
            await switchTo(a)
            await until(() => [...held].filter(item => item.controller === a).length >= 3, 'old controller reads are held')
            await switchTo(b)
            await nodes('b').waitFor()
            await providers('b').waitFor()
            await until(async () => (await core.textContent()).includes('controller-b-core'), 'returned controller version is current')
            const oldResponse = page.waitForResponse(response => new URL(response.url()).port === String(a.port) && new URL(response.url()).pathname === '/version')
            release(a, oldVersionStatus)
            a.holdState = false
            await oldResponse
            await page.waitForTimeout(150)
            assert.equal(await nodes('a').count(), 0, 'a late proxy response cannot replace current nodes')
            assert.equal(await providers('a').count(), 0, 'a late provider response cannot replace current nodes')
            assert.equal(await nodes('b').count(), 1)
            assert((await core.textContent()).includes('controller-b-core'), 'a late success or error cannot replace current version')
            assert.equal(await page.locator('.sidebar-version.is-offline').count(), 0, 'an obsolete authentication error cannot disconnect the current controller')
        }
    } finally {
        a.holdState = b.holdState = false
        release(a); release(b)
        await context.close()
    }
}

async function reloadLifecycle(current) {
    const { page, context } = await pageFor(current, 'settings')
    try {
        const editor = page.locator('.config-editor-content')
        const text = editor.locator('textarea')
        const close = editor.getByRole('button', { name: 'Close', exact: true })
        await page.getByRole('button', { name: 'Edit configuration file', exact: true }).click()
        await until(() => text.isEnabled(), 'configuration file is loaded')
        await text.fill('mode: global\n')
        current.reload = 'hang'
        await editor.getByRole('button', { name: 'Save and reload', exact: true }).click()
        await editor.getByText(/The file was saved, but a successful reload could not be confirmed/).waitFor()
        assert.equal(await text.inputValue(), 'mode: global\n', 'a reload timeout preserves the saved text')
        assert.equal(await close.isEnabled(), true, 'the editor unlocks after a reload timeout')
        assert.equal(await text.evaluate(element => element.readOnly), false)
        assert.equal(current.writes, 1)
        current.reload = 'normal'
        await editor.getByRole('button', { name: 'Retry reload', exact: true }).click()
        await editor.getByText('Configuration file saved and reloaded.', { exact: true }).waitFor()
        assert.equal(current.writes, 1, 'retry verifies the saved revision without rewriting unchanged content')
        await close.click()

        current.holdGeneralAfterReload = true
        const reload = page.getByRole('button', { name: 'Reload configuration', exact: true })
        await reload.click()
        await until(() => [...held].some(item => item.controller === current && item.path === '/configs'), 'settings refresh is held after a successful reload')
        await until(() => reload.isEnabled(), 'a settings refresh cannot leave a successful reload locked')
    } finally {
        current.reload = 'normal'
        current.holdGeneralAfterReload = current.generalHeld = false
        release(current)
        await context.close()
    }
}

async function ruleIsolation(a, b) {
    const { page, context } = await pageFor(a, 'rules')
    try {
        const rule = name => page.locator(`.rule-item .payload[title="rule-${name}.fixture"]`)
        const switchTo = current => page.evaluate(port => { location.hash = `/rules?host=127.0.0.1&port=${port}` }, current.port)
        await rule('a').waitFor()
        b.holdState = true
        await switchTo(b)
        await until(() => [...held].some(item => item.controller === b && item.path === '/rules'), 'switching controller starts a new rules request')
        assert.equal(await rule('a').count(), 0, 'rules from the previous controller are hidden while loading')
        b.holdState = false
        release(b)
        await rule('b').waitFor()

        a.holdState = true
        await switchTo(a)
        await until(() => [...held].some(item => item.controller === a && item.path === '/rules'), 'old rules request is held')
        await switchTo(b)
        await rule('b').waitFor()
        const oldResponse = page.waitForResponse(response => new URL(response.url()).port === String(a.port) && new URL(response.url()).pathname === '/rules')
        release(a)
        await oldResponse
        await page.waitForTimeout(150)
        assert.equal(await rule('a').count(), 0, 'a late rules response cannot replace current rules')
        assert.equal(await rule('b').count(), 1)
    } finally {
        a.holdState = b.holdState = false
        release(a); release(b)
        await context.close()
    }
}

async function main() {
    const a = controller('a')
    const b = controller('b')
    for (const current of controllers) {
        await new Promise(resolve => current.server.listen(0, '127.0.0.1', resolve))
        current.port = current.server.address().port
    }
    browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined, args: process.env.PLAYWRIGHT_CHROMIUM_ARGS ? JSON.parse(process.env.PLAYWRIGHT_CHROMIUM_ARGS) : [] })
    const failures = []
    for (const [name, check] of [['controller isolation', () => controllerIsolation(a, b)], ['rules isolation', () => ruleIsolation(a, b)], ['reload lifecycle', () => reloadLifecycle(a)]]) {
        try { await check(); console.log('PASS ' + name) } catch (error) { failures.push(name + ': ' + error.message) }
    }
    assert.deepEqual(pageErrors, [], 'no page errors')
    assert.deepEqual(failures, [], 'controller lifecycle checks')
    console.log(JSON.stringify({ passed: true, oldStateHidden: true, lateResponsesIgnored: true, rulesFetchOnControllerChange: true, reloadTimeoutUnlocksEditor: true, reloadRetryPreservesRevision: true, refreshDoesNotBlockReload: true }))
}

main().catch(error => { console.error(error); process.exitCode = 1 }).finally(async () => {
    await browser?.close()
    for (const current of controllers) {
        current.server.closeAllConnections()
        await new Promise(resolve => current.server.close(resolve))
    }
})
