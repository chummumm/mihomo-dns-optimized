#!/usr/bin/env node
// Production-build UI checks against an isolated local controller fixture.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const path = require('node:path')
const { WebSocketServer } = require('ws')
const { chromium } = require('playwright')

const dist = path.resolve(process.env.UI_DIST_DIR || path.join(__dirname, '../dist'))
const defaultURL = 'https://www.gstatic.com/generate_204'
const customURL = 'https://probe.example.test/generate_204?check=1'
const config = { port: 7890, 'socks-port': 7891, 'mixed-port': 0, 'redir-port': 0, mode: 'rule', 'log-level': 'info', 'allow-lan': false }
const direct = { name: 'DIRECT', type: 'Direct', history: [] }
const pass = { name: 'PASS', type: 'Pass', history: [] }
const leaf = { name: 'node-a', type: 'Socks', history: [], udp: true }
const group = { name: 'select-group', type: 'Selector', now: 'node-a', history: [], all: ['DIRECT', 'PASS', 'node-a'] }
const delayRequests = []
const reloadRequests = []
const reloadTargets = []
const externalRequests = []
const pageErrors = []
const reactWarnings = []
let reloadMode = 'hold'
let heldReload
let onReload
let holdNextDelay = false
let heldDelay
let onDelay
function handleRequest(req, res) {
    res.setHeader('Access-Control-Allow-Origin', 'http://127.0.0.1:' + server.address().port)
    res.setHeader('Access-Control-Allow-Headers', 'Authorization, Content-Type')
    res.setHeader('Access-Control-Allow-Methods', 'GET, PUT, PATCH, DELETE, OPTIONS')
    if (req.method === 'OPTIONS') { res.writeHead(204); res.end(); return }
    const url = new URL(req.url, 'http://127.0.0.1')
    const pathname = url.pathname
    const secondary = req.socket.localPort === secondServer.address()?.port
    if (pathname === '/configs' && req.method === 'PUT') {
        let body = ''
        req.on('data', chunk => { body += chunk })
        req.on('end', () => {
            reloadRequests.push({ body: JSON.parse(body), query: url.search })
            reloadTargets.push(req.socket.localPort)
            if (reloadMode === 'hold') heldReload = res
            else {
                res.writeHead(400, { 'content-type': 'application/json' })
                res.end(JSON.stringify({ message: '<b>fixture config error</b>' }))
            }
            onReload?.()
        })
        return
    }
    const delay = pathname.match(/^\/proxies\/(.+)\/delay$/)
    if (delay) {
        delayRequests.push({ name: decodeURIComponent(delay[1]), url: url.searchParams.get('url'), timeout: url.searchParams.get('timeout') })
        if (holdNextDelay) {
            holdNextDelay = false
            heldDelay = res
            onDelay?.()
            return
        }
        res.writeHead(200, { 'content-type': 'application/json' })
        res.end(JSON.stringify({ delay: secondary ? 44 : 18 })); return
    }
    const api = {
        '/version': { version: 'isolated-ui-features', premium: false },
        '/configs': config, '/rules': { rules: [] },
        '/connections': { connections: [], uploadTotal: 0, downloadTotal: 0 },
        '/providers/rules': { providers: {} },
        // PASS is intentionally present in a provider as well as GLOBAL. A
        // per-card guard must still prevent a request for this built-in policy.
        '/providers/proxies': { providers: { fixture: { name: 'fixture', type: 'Proxy', vehicleType: 'File', proxies: [pass] } } },
        '/proxies': { proxies: {
            GLOBAL: { name: 'GLOBAL', type: 'Selector', now: 'node-a', history: [], all: ['DIRECT', 'REJECT', 'PASS', 'node-a', 'select-group'] },
            DIRECT: direct, REJECT: { name: 'REJECT', type: 'Reject', history: [] }, PASS: pass,
            'node-a': secondary ? { ...leaf, history: [{ time: '2026-01-01T00:00:00Z', delay: 44 }] } : leaf, 'select-group': group,
        } },
    }
    if (api[pathname]) {
        res.writeHead(200, { 'content-type': 'application/json' })
        res.end(JSON.stringify(api[pathname])); return
    }
    const file = path.join(dist, pathname === '/' ? 'index.html' : pathname)
    if (!file.startsWith(dist + path.sep) || !fs.existsSync(file) || !fs.statSync(file).isFile()) {
        res.writeHead(404); res.end(); return
    }
    const types = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.png': 'image/png', '.svg': 'image/svg+xml', '.webmanifest': 'application/manifest+json' }
    res.writeHead(200, { 'content-type': types[path.extname(file)] || 'application/octet-stream' })
    fs.createReadStream(file).pipe(res)
}
const server = http.createServer(handleRequest)
const secondServer = http.createServer(handleRequest)
const wss = new WebSocketServer({ server })
const secondWss = new WebSocketServer({ server: secondServer })
function handleSocket(socket, req) {
    if (new URL(req.url, 'http://127.0.0.1').pathname === '/connections') {
        socket.send(JSON.stringify({ connections: [], uploadTotal: 0, downloadTotal: 0 }))
    }
}
wss.on('connection', handleSocket)
secondWss.on('connection', handleSocket)

let browser
async function main() {
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
    await new Promise(resolve => secondServer.listen(0, '127.0.0.1', resolve))
    const port = server.address().port
    const secondPort = secondServer.address().port
    browser = await chromium.launch({
        headless: true,
        executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined,
        args: process.env.PLAYWRIGHT_CHROMIUM_ARGS ? JSON.parse(process.env.PLAYWRIGHT_CHROMIUM_ARGS) : [],
    })
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'en-US', serviceWorkers: 'block' })
    await context.route('**/*', route => {
        const url = new URL(route.request().url())
        if (url.hostname === '127.0.0.1' && [String(port), String(secondPort)].includes(url.port)) return route.continue()
        externalRequests.push({ url: url.origin + url.pathname, type: route.request().resourceType() })
        return route.abort()
    })
    await context.addInitScript(({ port }) => {
        localStorage.setItem('language', JSON.stringify('en_US'))
        localStorage.setItem('externalControllers', JSON.stringify([{ hostname: '127.0.0.1', port, secret: '' }]))
        localStorage.setItem('externalControllerIndex', '0')
        // An existing profile predates the new setting. Its other fields survive.
        if (!localStorage.getItem('profile')) localStorage.setItem('profile', JSON.stringify({ breakConnections: true, logLevel: '' }))
    }, { port })
    const page = await context.newPage()
    page.setDefaultTimeout(10000)
    page.on('pageerror', error => pageErrors.push(String(error)))
    page.on('console', message => {
        if (/unmountComponentAtNode|ReactDOM\.render|synchronously unmount|unmounted component/.test(message.text())) reactWarnings.push(message.text())
    })
    await page.goto('http://127.0.0.1:' + port + '/#/settings', { waitUntil: 'networkidle' })
    const probe = page.getByRole('textbox', { name: 'Speed test URL', exact: true })
    assert.equal(await probe.inputValue(), defaultURL, 'existing profiles receive the default')
    const waitMessageRemoved = () => page.waitForFunction(() => document.querySelectorAll('.message-container').length === 0)
    async function navigate(name) {
        await page.getByRole('link', { name, exact: true }).click()
        await page.waitForLoadState('networkidle')
    }
    async function speedTest(expectedURL) {
        await navigate('Proxies')
        await page.locator('.proxy-name').filter({ hasText: /^node-a$/ }).waitFor()
        const previous = delayRequests.length
        const response = page.waitForResponse(response => new URL(response.url()).pathname === '/proxies/node-a/delay')
        await page.getByText('Speed Test', { exact: true }).click()
        await response
        await page.locator('.proxy-item').filter({ hasText: 'node-a' }).getByText('18ms', { exact: true }).waitFor()
        assert.deepEqual(delayRequests.slice(previous), [{ name: 'node-a', url: expectedURL, timeout: '5000' }], 'only the real node is tested, with the configured URL')
    }
    await speedTest(defaultURL)
    await navigate('Setting')
    await probe.fill(customURL)
    await page.getByRole('button', { name: 'Save', exact: true }).click()
    await page.getByRole('status').getByText('Speed test URL saved', { exact: true }).waitFor()
    await waitMessageRemoved()
    const profile = await page.evaluate(() => JSON.parse(localStorage.getItem('profile')))
    assert.equal(profile.probeURL, customURL)
    assert.equal(profile.breakConnections, true, 'existing preferences survive a new URL save')
    await speedTest(customURL)
    await navigate('Setting')
    await probe.fill('ftp://probe.example.test/204')
    await page.getByRole('button', { name: 'Save', exact: true }).click()
    await page.getByRole('alert').getByText('Enter an HTTP or HTTPS URL without credentials or a fragment.', { exact: true }).waitFor()
    assert.equal(await page.evaluate(() => JSON.parse(localStorage.getItem('profile')).probeURL), customURL, 'invalid URLs do not replace the saved URL')
    await waitMessageRemoved()
    await page.getByRole('button', { name: 'Use default', exact: true }).click()
    assert.equal(await probe.inputValue(), defaultURL)

    let received = new Promise(resolve => { onReload = resolve })
    await page.getByRole('button', { name: 'Reload configuration', exact: true }).click()
    await received
    const pending = page.getByRole('button', { name: 'Reloading…', exact: true })
    assert.equal(await pending.isDisabled(), true)
    await pending.evaluate(button => { button.click(); button.click() })
    assert.equal(reloadRequests.length, 1, 'in-flight reloads cannot be duplicated')
    assert.deepEqual(reloadRequests[0], { body: {}, query: '' }, 'reload uses the current path without supplying or forcing a config')
    config['mixed-port'] = 7893
    heldReload.writeHead(204); heldReload.end(); heldReload = null
    await page.getByRole('status').getByText('Configuration reloaded', { exact: true }).waitFor()
    await page.waitForFunction(() => [...document.querySelectorAll('input')].some(input => input.value === '7893'))
    await waitMessageRemoved()

    await page.emulateMedia({ reducedMotion: 'reduce' })
    reloadMode = 'error'
    await page.getByRole('button', { name: 'Reload configuration', exact: true }).click()
    await page.getByRole('alert').getByText('Unable to reload configuration: <b>fixture config error</b>', { exact: true }).waitFor()
    assert.equal(await page.locator('.message-content b').count(), 0, 'controller errors are text')
    await waitMessageRemoved()
    assert.equal(await page.getByRole('button', { name: 'Reload configuration', exact: true }).isDisabled(), false)

    reloadMode = 'hold'
    received = new Promise(resolve => { onReload = resolve })
    await page.getByRole('button', { name: 'Reload configuration', exact: true }).click()
    await received
    await navigate('Proxies')
    heldReload.writeHead(204); heldReload.end(); heldReload = null
    await page.waitForLoadState('networkidle')
    assert.equal(await page.locator('.message-container').count(), 0, 'leaving settings aborts UI callbacks from an obsolete reload')
    await navigate('Setting')
    assert.equal(await page.getByRole('button', { name: 'Reload configuration', exact: true }).isDisabled(), false)

    await navigate('Proxies')
    holdNextDelay = true
    const delayReceived = new Promise(resolve => { onDelay = resolve })
    await page.getByText('Speed Test', { exact: true }).click()
    await delayReceived
    const canceled = page.waitForEvent('requestfailed', {
        predicate: request => new URL(request.url()).pathname === '/proxies/node-a/delay' && new URL(request.url()).port === String(port),
    })
    const newController = page.waitForResponse(response => new URL(response.url()).pathname === '/proxies' && new URL(response.url()).port === String(secondPort))
    await page.evaluate(secondPort => { location.hash = '/proxies?port=' + secondPort + '&protocol=http:' }, secondPort)
    await Promise.all([canceled, newController])
    heldDelay.writeHead(200, { 'content-type': 'application/json' }); heldDelay.end(JSON.stringify({ delay: 999 })); heldDelay = null
    await page.locator('.proxy-item').filter({ hasText: 'node-a' }).getByText('44ms', { exact: true }).waitFor()
    assert.equal(await page.getByText('999ms', { exact: true }).count(), 0, 'old controller results cannot overwrite the new controller same-name node')
    await navigate('Setting')
    await page.getByText('http://127.0.0.1:' + secondPort, { exact: true }).waitFor()
    await page.getByText('Controller specified by the current link.', { exact: true }).waitFor()
    assert.equal(await page.locator('.settings-card .external-controller').count(), 0, 'a URL-selected controller has no ineffective saved-controller edit control')
    received = new Promise(resolve => { onReload = resolve })
    await page.getByRole('button', { name: 'Reload configuration', exact: true }).click()
    await received
    assert.equal(reloadTargets.at(-1), secondPort, 'reload targets the controller displayed for the current URL')
    heldReload.writeHead(204); heldReload.end(); heldReload = null
    await page.getByRole('status').getByText('Configuration reloaded', { exact: true }).waitFor()
    await waitMessageRemoved()
    await page.setViewportSize({ width: 320, height: 740 })
    const inputRect = await probe.boundingBox()
    assert.ok(inputRect.x >= 0 && inputRect.x + inputRect.width <= 320, 'URL form fits a small phone')
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, 'settings has no mobile body overflow')
    await page.getByText('中文', { exact: true }).click()
    await page.getByRole('button', { name: '重新加载配置', exact: true }).waitFor()
    await page.getByRole('textbox', { name: '测速 URL', exact: true }).waitFor()
    assert.deepEqual(pageErrors, [])
    assert.deepEqual(reactWarnings, [])
    assert.ok(externalRequests.every(request => request.type === 'font' && request.url === 'http://at.alicdn.com/t/font_841708_ok9czskbhel.ttf'), 'no fixture or speed-test URL is contacted directly by the browser')
    console.log(JSON.stringify({ passed: true, browser: browser.version(), delayRequests, reloadCount: reloadRequests.length, controllerSwitchCancelsOldProbe: true, reloadMatchesDisplayedURLController: true, messageRootsAfterClose: 0, mobileWidth: 320, localeSwitch: true, pageErrors, reactWarnings }, null, 2))
}

main().catch(error => { console.error(error.stack); process.exitCode = 1 }).finally(async () => {
    if (browser) await browser.close()
    for (const socket of wss.clients) socket.terminate()
    for (const socket of secondWss.clients) socket.terminate()
    wss.close(); secondWss.close(); server.close(); secondServer.close()
})
