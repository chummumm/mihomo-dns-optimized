#!/usr/bin/env node
// The browser must never manage core or dashboard updates. Exercise real UI
// routes and retained editor actions against two isolated controllers.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const path = require('node:path')
const { chromium } = require('playwright')
const { WebSocketServer } = require('ws')

const dist = path.resolve(process.env.UI_DIST_DIR || path.join(__dirname, '../dist'))
const file = { path: '/fixture/config.yaml', content: 'mode: rule\n# retained-editor-fixture\n', revision: 'revision-1', max_size: 4 * 1024 * 1024 }
const requests = []
const upgrades = []
const saves = []
const reloads = []
const errors = []
let browser
let navigations = 0

function json(res, status, body) {
    res.writeHead(status, { 'content-type': 'application/json', 'cache-control': 'no-store' })
    res.end(JSON.stringify(body))
}

function handle(req, res) {
    const url = new URL(req.url, 'http://127.0.0.1')
    requests.push({ port: req.socket.localPort, path: url.pathname, method: req.method })
    res.setHeader('Access-Control-Allow-Origin', 'http://127.0.0.1:' + first.address().port)
    res.setHeader('Access-Control-Allow-Headers', 'Authorization, Content-Type')
    res.setHeader('Access-Control-Allow-Methods', 'GET, PUT, PATCH, OPTIONS')
    if (url.pathname === '/upgrade' || url.pathname.startsWith('/upgrade/')) {
        upgrades.push({ path: url.pathname, method: req.method })
        json(res, 410, { message: 'Browser update operations are unavailable' }); return
    }
    if (req.method === 'OPTIONS') { res.writeHead(204); res.end(); return }
    if (url.pathname === '/configs/file' && req.method === 'PUT') {
        let body = ''
        req.on('data', chunk => { body += chunk })
        req.on('end', () => {
            const payload = JSON.parse(body)
            saves.push(payload)
            file.content = payload.content
            file.revision = 'revision-2'
            json(res, 200, file)
        })
        return
    }
    if (url.pathname === '/configs' && req.method !== 'GET') {
        reloads.push(req.method)
        res.writeHead(204); res.end(); return
    }
    const api = {
        '/version': { version: 'mihomo-optimized-v1.0.0', premium: false },
        '/configs': { port: 7890, 'socks-port': 7891, 'mixed-port': 0, 'redir-port': 0, mode: 'rule', 'log-level': 'info', 'allow-lan': false },
        '/configs/file': file,
        '/connections': { connections: [], uploadTotal: 0, downloadTotal: 0 },
        '/dns/observability': { version: 1, storage: 'memory', enabled: false, memory_limit_bytes: 8 * 1024 * 1024, accounted_bytes: 0 },
        '/rules': { rules: [] }, '/providers/rules': { providers: {} }, '/providers/proxies': { providers: {} },
        '/proxies': { proxies: { GLOBAL: { name: 'GLOBAL', type: 'Selector', now: 'DIRECT', all: ['DIRECT'], history: [] }, DIRECT: { name: 'DIRECT', type: 'Direct', history: [] } } },
    }
    if (api[url.pathname]) { json(res, 200, api[url.pathname]); return }
    const relative = url.pathname.startsWith('/ui/') ? url.pathname.slice(4) : url.pathname.slice(1)
    const filename = path.resolve(dist, relative || 'index.html')
    if (!filename.startsWith(dist + path.sep) || !fs.existsSync(filename) || !fs.statSync(filename).isFile()) { res.writeHead(404); res.end(); return }
    const types = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml', '.png': 'image/png', '.webmanifest': 'application/manifest+json' }
    res.writeHead(200, { 'content-type': types[path.extname(filename)] || 'application/octet-stream', 'cache-control': 'no-store' })
    fs.createReadStream(filename).pipe(res)
}

const first = http.createServer(handle)
const second = http.createServer(handle)
const sockets = [new WebSocketServer({ server: first }), new WebSocketServer({ server: second })]
for (const wss of sockets) {
    wss.on('connection', (socket, req) => {
        const pathname = new URL(req.url, 'http://127.0.0.1').pathname
        socket.send(JSON.stringify(pathname === '/logs'
            ? { type: 'info', payload: 'retained-log-fixture' }
            : pathname === '/traffic' ? { up: 0, down: 0 } : { connections: [], uploadTotal: 0, downloadTotal: 0 }))
    })
}

async function main() {
    await Promise.all([new Promise(resolve => first.listen(0, '127.0.0.1', resolve)), new Promise(resolve => second.listen(0, '127.0.0.1', resolve))])
    const firstPort = first.address().port
    const secondPort = second.address().port
    browser = await chromium.launch({
        headless: true,
        executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined,
        args: process.env.PLAYWRIGHT_CHROMIUM_ARGS ? JSON.parse(process.env.PLAYWRIGHT_CHROMIUM_ARGS) : [],
    })
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'en-US', serviceWorkers: 'block' })
    await context.route('**/*', route => {
        const url = new URL(route.request().url())
        if (url.hostname === '127.0.0.1' && [String(firstPort), String(secondPort)].includes(url.port)) return route.continue()
        return route.abort()
    })
    await context.addInitScript(({ firstPort }) => {
        if (!localStorage.getItem('language')) localStorage.setItem('language', JSON.stringify('en_US'))
        localStorage.setItem('externalControllers', JSON.stringify([{ hostname: '127.0.0.1', port: firstPort, secret: '' }]))
        localStorage.setItem('externalControllerIndex', '0')
        // Retired browser preferences cannot revive removed update tasks.
        localStorage.setItem('profile', JSON.stringify({ autoUpdateDashboard: true, autoUpdateCore: true }))
    }, { firstPort })
    const page = await context.newPage()
    page.setDefaultTimeout(10000)
    await page.clock.install()
    const attemptedUpgrades = []
    page.on('pageerror', reason => errors.push(String(reason)))
    page.on('request', request => {
        const pathname = new URL(request.url()).pathname
        if (pathname === '/upgrade' || pathname.startsWith('/upgrade/')) attemptedUpgrades.push(request.method() + ' ' + pathname)
    })
    page.on('framenavigated', frame => { if (frame === page.mainFrame()) navigations++ })
    const forbiddenLabels = /^(Dashboard updates|Core updates|Check for updates|Check Update|Update now|Install manually|Refresh dashboard|Open core dashboard|Core automatic updates|Download updates automatically|Check interval|Next scheduled check|面板更新|内核更新|检查更新|立即更新|手动安装|刷新面板|打开内核面板|内核自动更新|自动拉取更新|检查间隔|下次计划检查)$/
    const noControls = async () => {
        assert.equal(await page.getByText(forbiddenLabels, { exact: true }).count(), 0, 'no update controls or status are rendered')
        assert.equal(await page.locator('.dashboard-update-settings, .dashboard-update-notice').count(), 0)
        assert.equal(await page.locator('input').evaluateAll(inputs => inputs.filter(input => /external-ui|dashboard-(path|url)/i.test([input.name, input.id, input.placeholder].join(' '))).length), 0, 'no dashboard path or download URL can be entered')
        assert.deepEqual(attemptedUpgrades, [], 'the browser never attempts an update API request')
        assert.deepEqual(upgrades, [], 'neither controller receives an update API request')
    }

    await page.goto('http://127.0.0.1:' + firstPort + '/ui/#/settings', { waitUntil: 'networkidle' })
    assert.equal(await page.title(), 'mihomo-optimized')
    assert.equal(await page.locator('.sidebar-name').textContent(), 'mihomo-optimized')
    await noControls()
    const editor = page.locator('.config-editor-content')
    const text = editor.locator('textarea')
    await page.getByRole('button', { name: 'Edit configuration file', exact: true }).click()
    await page.waitForFunction(() => document.querySelector('.config-editor-text')?.value === 'mode: rule\n# retained-editor-fixture\n')
    const draft = 'mode: global\n# preserved-unsaved-draft\n'
    await text.fill(draft)
    const beforeClock = navigations
    await page.clock.fastForward(49 * 60 * 60 * 1000)
    await page.evaluate(() => { document.dispatchEvent(new Event('visibilitychange')); window.dispatchEvent(new Event('offline')); window.dispatchEvent(new Event('online')) })
    await page.waitForTimeout(80)
    assert.equal(await text.inputValue(), draft, 'long-running pages and network changes preserve the draft')
    assert.equal(navigations, beforeClock, 'no updater refresh interrupts configuration editing')
    await noControls()
    await editor.getByRole('button', { name: 'Close', exact: true }).click()
    await editor.getByRole('button', { name: 'Keep editing', exact: true }).click()
    assert.equal(await text.inputValue(), draft, 'the existing discard guard remains active')
    await text.press('Control+s')
    await editor.getByText('Configuration file saved. The running configuration is unchanged. Use Reload configuration to apply your changes.', { exact: true }).waitFor()
    assert.deepEqual(saves, [{ content: draft, revision: 'revision-1' }])
    assert.deepEqual(reloads, [], 'saving remains separate from runtime configuration changes')
    await editor.getByRole('button', { name: 'Close', exact: true }).click()
    await editor.waitFor({ state: 'detached' })
    await page.getByRole('button', { name: 'Reload configuration', exact: true }).click()
    await page.waitForFunction(() => !document.querySelector('.settings-page .button-loading'))
    assert.deepEqual(reloads, ['PUT'], 'explicit configuration reload remains available')

    for (const [route, title] of [['proxies', 'Proxies'], ['connections', 'Connections'], ['dns', 'DNS'], ['logs', 'Logs'], ['rules', 'Rules'], ['settings', 'Settings']]) {
        await page.evaluate(route => { location.hash = '/' + route }, route)
        await page.getByRole('heading', { name: title, level: 1, exact: true }).waitFor()
        await page.waitForTimeout(60)
        await noControls()
    }
    await page.evaluate(secondPort => { location.hash = '/settings?port=' + secondPort + '&protocol=http:' }, secondPort)
    await page.waitForFunction(port => document.querySelector('.controller-endpoint')?.textContent.includes(':' + port), secondPort)
    await page.waitForTimeout(80)
    assert.ok(requests.some(request => request.port === secondPort && request.path === '/configs'), 'controller switching still reads the selected core')
    await noControls()
    await page.evaluate(() => localStorage.setItem('language', JSON.stringify('zh_CN')))
    await page.reload({ waitUntil: 'networkidle' })
    await page.getByRole('button', { name: '编辑配置文件', exact: true }).waitFor()
    await noControls()
    assert.equal(await page.evaluate(() => Object.values(localStorage).join('\n').includes('preserved-unsaved-draft')), false)
    assert.deepEqual(errors, [])
    console.log(JSON.stringify({ passed: true, controlsRemovedInBothLanguages: true, routesChecked: 6, simulatedHours: 49, controllerSwitchPreserved: true, editorDraftPreserved: true, configSaveAndReloadPreserved: true, updateRequests: attemptedUpgrades.length, pageErrors: errors }, null, 2))
}

main().catch(error => { console.error(error.stack); process.exitCode = 1 }).finally(async () => {
    if (browser) await browser.close()
    for (const wss of sockets) { for (const socket of wss.clients) socket.terminate(); wss.close() }
    first.close(); second.close()
})
