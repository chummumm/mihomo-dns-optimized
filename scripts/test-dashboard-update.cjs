#!/usr/bin/env node
const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const path = require('node:path')
const { chromium } = require('playwright')
const { WebSocketServer } = require('ws')

const dist = path.resolve(process.env.UI_DIST_DIR || path.join(__dirname, '../dist'))
let browser

async function serviceWorkerMigration() {
    let genericInstalled = false
    let networkNavigations = 0
    const oldHTML = '<!doctype html><title>Old dashboard</title><h1>old precached dashboard</h1><script>navigator.serviceWorker.register("/ui/sw.js", {scope:"/ui/"}).catch(()=>{})</script>'
    const newHTML = '<!doctype html><title>Generic dashboard</title><h1>generic dashboard without a service worker</h1>'
    const worker = `
        self.addEventListener('install', event => event.waitUntil(caches.open('fixture-old-ui-precache').then(cache => cache.add('/ui/index.html')).then(() => self.skipWaiting())));
        self.addEventListener('activate', event => event.waitUntil(self.clients.claim()));
        self.addEventListener('fetch', event => {
            if (event.request.mode === 'navigate' && new URL(event.request.url).pathname.startsWith('/ui/')) event.respondWith(caches.match('/ui/index.html'));
        });
    `
    const server = http.createServer((req, res) => {
        res.setHeader('Cache-Control', 'no-store')
        if (req.url === '/other/sw.js') {
            res.writeHead(200, { 'content-type': 'text/javascript' }); res.end("self.addEventListener('install',()=>self.skipWaiting())"); return
        }
        if (req.url === '/ui/sw.js') {
            res.writeHead(genericInstalled ? 404 : 200, { 'content-type': 'text/javascript' }); res.end(genericInstalled ? '' : worker); return
        }
        if (req.url === '/ui/' || req.url === '/ui/index.html') {
            networkNavigations++
            res.writeHead(200, { 'content-type': 'text/html' }); res.end(genericInstalled ? newHTML : oldHTML); return
        }
        res.writeHead(404); res.end()
    })
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
    const context = await browser.newContext({ serviceWorkers: 'allow' })
    try {
        const page = await context.newPage()
        const origin = 'http://127.0.0.1:' + server.address().port
        await page.goto(origin + '/ui/')
        await page.evaluate(async () => {
            await navigator.serviceWorker.ready
            await navigator.serviceWorker.register('/other/sw.js', { scope: '/other/' })
            const unrelated = await caches.open('fixture-unrelated-cache')
            await unrelated.put('/other/resource', new Response('preserve me'))
        })
        await page.waitForFunction(() => navigator.serviceWorker.controller !== null)
        genericInstalled = true
        const beforeStaleReload = networkNavigations
        await page.reload()
        assert.equal(await page.locator('h1').textContent(), 'old precached dashboard', 'the old UI worker actually intercepts navigation to the newly installed generic UI')
        assert.equal(networkNavigations, beforeStaleReload, 'the stale reload came from the worker precache, not the server')
        assert.equal(await page.evaluate(async () => {
            const registration = await navigator.serviceWorker.getRegistration()
            if (registration.scope !== location.origin + '/ui/') return false
            return await registration.unregister()
        }), true)
        await page.reload()
        assert.equal(await page.locator('h1').textContent(), 'generic dashboard without a service worker')
        assert.equal(await page.evaluate(() => navigator.serviceWorker.controller === null), true, 'the fresh navigation is no longer controlled by the unregistered old worker')
        assert.equal(await page.evaluate(async () => (await navigator.serviceWorker.getRegistrations()).some(registration => registration.scope === location.origin + '/other/')), true, 'unrelated worker scope survives')
        assert.equal(await page.evaluate(async () => (await caches.open('fixture-unrelated-cache')).match('/other/resource').then(response => response.text())), 'preserve me', 'unrelated cache survives')
        return { oldPrecacheReproduced: true, unregisterReloadReachedGenericUI: true, unrelatedWorkerAndCachePreserved: true }
    } finally {
        await context.close()
        await new Promise(resolve => server.close(resolve))
    }
}

async function updaterControls(versionOnly = false) {
    const firstStatus = { supported: true, update_available: false, updating: false, reason: 'version-unavailable', current_version: '', current_commit: '', current_digest: 'a'.repeat(64), latest_digest: 'b'.repeat(64), auto_update: false, update_interval: 24 }
    if (versionOnly) Object.assign(firstStatus, { current_version: '0.3.7', latest_version: '0.3.7', reason: undefined })
    const secondStatus = { supported: true, update_available: false, updating: false, current_version: '', current_commit: '', current_digest: 'd'.repeat(64), latest_digest: 'd'.repeat(64) }
    const configFile = { path: '/fixture/config.yaml', content: 'mode: rule\n# editor-privacy-fixture\n', revision: 'a'.repeat(64), max_size: 4 * 1024 * 1024 }
    const statusRequests = []
    const installations = []
    const runtimeWrites = []
    const pageErrors = []
    let mode = 'normal'
    let holdInstall = true
    let heldCheck
    let heldInstall
    let navigations = 0
    function json(res, status, body) {
        res.writeHead(status, { 'content-type': 'application/json', 'cache-control': 'no-store' })
        res.end(JSON.stringify(body))
    }
    function handler(req, res) {
        res.setHeader('Access-Control-Allow-Origin', 'http://127.0.0.1:' + first.address().port)
        res.setHeader('Access-Control-Allow-Headers', 'Authorization, Content-Type')
        res.setHeader('Access-Control-Allow-Methods', 'GET, POST, PUT, PATCH, OPTIONS')
        if (req.method === 'OPTIONS') { res.writeHead(204); res.end(); return }
        const url = new URL(req.url, 'http://127.0.0.1')
        const secondary = req.socket.localPort === second.address()?.port
        const status = secondary ? secondStatus : firstStatus
        if (url.pathname === '/upgrade/ui' && req.method === 'GET') {
            const check = url.searchParams.get('check') === 'true'
            statusRequests.push({ port: req.socket.localPort, check })
            if (!secondary && check && mode === 'hold-check') { heldCheck = res; return }
            if (mode === 'unsupported') { json(res, 404, { message: 'Not found' }); return }
            json(res, 200, status); return
        }
        if (url.pathname === '/upgrade/ui' && req.method === 'POST') {
            installations.push(req.socket.localPort)
            if (holdInstall) { heldInstall = res; return }
            status.current_digest = status.latest_digest
            status.update_available = false
            json(res, 200, { status: 'ok' }); return
        }
        if (['/configs', '/configs/file'].includes(url.pathname) && req.method !== 'GET') {
            runtimeWrites.push(req.method + ' ' + url.pathname)
            json(res, 200, {}); return
        }
        const api = {
            '/version': { version: 'isolated-updater-fixture', premium: false },
            '/configs': { port: 7890, 'socks-port': 7891, 'mixed-port': 0, 'redir-port': 0, mode: 'rule', 'log-level': 'info', 'allow-lan': false },
            '/configs/file': configFile,
            '/connections': { connections: [], uploadTotal: 0, downloadTotal: 0 },
            '/rules': { rules: [] }, '/providers/rules': { providers: {} }, '/providers/proxies': { providers: {} }, '/proxies': { proxies: {} },
        }
        if (api[url.pathname]) { json(res, 200, api[url.pathname]); return }
        const relative = url.pathname.startsWith('/ui/') ? url.pathname.slice(4) : url.pathname.slice(1)
        const filename = path.join(dist, relative || 'index.html')
        if (!filename.startsWith(dist + path.sep) || !fs.existsSync(filename) || !fs.statSync(filename).isFile()) { res.writeHead(404); res.end(); return }
        const types = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml', '.png': 'image/png', '.webmanifest': 'application/manifest+json' }
        res.writeHead(200, { 'content-type': types[path.extname(filename)] || 'application/octet-stream', 'cache-control': 'no-store' })
        fs.createReadStream(filename).pipe(res)
    }
    const first = http.createServer(handler)
    const second = http.createServer(handler)
    const sockets = [new WebSocketServer({ server: first }), new WebSocketServer({ server: second })]
    for (const wss of sockets) wss.on('connection', socket => socket.send(JSON.stringify({ connections: [], uploadTotal: 0, downloadTotal: 0 })))
    await Promise.all([new Promise(resolve => first.listen(0, '127.0.0.1', resolve)), new Promise(resolve => second.listen(0, '127.0.0.1', resolve))])
    const firstPort = first.address().port
    const secondPort = second.address().port
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'en-US', serviceWorkers: 'block' })
    async function received(check) {
        const deadline = Date.now() + 3000
        while (!check() && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 10))
        assert.ok(check(), 'the controller fixture received the expected updater request')
    }
    try {
        await context.route('**/*', route => {
            const url = new URL(route.request().url())
            if (url.hostname === '127.0.0.1' && [String(firstPort), String(secondPort)].includes(url.port)) return route.continue()
            return route.abort()
        })
        await context.addInitScript(({ firstPort }) => {
            localStorage.setItem('language', JSON.stringify('en_US'))
            localStorage.setItem('externalControllers', JSON.stringify([{ hostname: '127.0.0.1', port: firstPort, secret: '' }]))
            localStorage.setItem('externalControllerIndex', '0')
            // A saved 0.3.6 preference must never schedule checks or installs.
            if (!localStorage.getItem('profile')) localStorage.setItem('profile', JSON.stringify({ autoUpdateDashboard: true }))
            const realNow = Date.now
            window.__updateClockOffset = 0
            Date.now = () => realNow() + window.__updateClockOffset
        }, { firstPort })
        const page = await context.newPage()
        page.setDefaultTimeout(10000)
        page.on('pageerror', error => pageErrors.push(String(error)))
        page.on('framenavigated', frame => { if (frame === page.mainFrame()) navigations++ })
        await page.goto('http://127.0.0.1:' + firstPort + '/ui/#/settings', { waitUntil: 'networkidle' })
        const settings = page.locator('.dashboard-update-settings')
        const checkButton = () => settings.getByRole('button', { name: 'Check for updates', exact: true })
        const automatic = () => settings.locator('.settings-row').filter({ hasText: 'Core automatic updates' })
        const editor = page.locator('.config-editor-content')
        const text = editor.locator('textarea')
        const openEditor = async () => {
            await page.getByRole('button', { name: 'Edit configuration file', exact: true }).click()
            await page.waitForFunction(() => document.querySelector('.config-editor-text')?.value === 'mode: rule\n# editor-privacy-fixture\n')
        }
        const closeEditor = async () => {
            await editor.getByRole('button', { name: 'Close', exact: true }).click()
            const discard = editor.getByRole('button', { name: 'Discard and close', exact: true })
            if (await discard.count()) await discard.click()
            await editor.waitFor({ state: 'detached' })
        }
        await checkButton().waitFor()
        await received(() => statusRequests.length > 0)
        assert.equal(statusRequests[0].check, false, 'opening only reads local status, even with the obsolete browser preference enabled')
        assert.equal(statusRequests.some(request => request.check), false)
        assert.equal(installations.length, 0)
        assert.equal(await settings.getByRole('switch').count(), 0, 'the dashboard exposes no automatic-update setting')
        assert.match(await automatic().textContent(), /Disabled/, 'automatic-update status comes from core YAML, not the saved browser preference')
        assert.equal(await settings.getByText('24 hours', { exact: true }).count(), 1)
        const openingRequests = statusRequests.length
        await page.evaluate(() => { window.__updateClockOffset += 48 * 60 * 60 * 1000; document.dispatchEvent(new Event('visibilitychange')); window.dispatchEvent(new Event('online')) })
        await page.waitForTimeout(40)
        assert.equal(statusRequests.length, openingRequests, 'visibility and network events never schedule dashboard update requests')
        assert.deepEqual(installations, [])

        if (versionOnly) {
            const updateButton = () => settings.getByRole('button', { name: 'Update now', exact: true })
            const manualButton = () => settings.getByRole('button', { name: 'Install manually', exact: true })
            const checkStatus = async () => {
                await checkButton().click()
                await page.waitForFunction(() => !document.querySelector('.dashboard-update-settings .button-loading'))
            }
            assert.equal(await updateButton().count(), 0, 'equal versions with different digests do not offer an update')
            assert.equal(await manualButton().count(), 0, 'equal versions do not offer a manual downgrade or reinstall')
            firstStatus.latest_version = '0.3.6'
            await checkStatus()
            assert.equal(await updateButton().count(), 0, 'a lower source version does not offer an update')
            assert.equal(await manualButton().count(), 0)
            firstStatus.latest_version = '0.3.8'
            firstStatus.update_available = true
            await checkStatus()
            assert.equal(await updateButton().count(), 1, 'a newer source version offers the regular update action')
            assert.deepEqual(installations, [], 'checking newer versions never installs from the browser')
            firstStatus.latest_version = ''
            firstStatus.reason = 'version-unavailable'
            firstStatus.update_available = false
            await checkStatus()
            assert.equal(await updateButton().count(), 0)
            assert.equal(await manualButton().count(), 1)
            assert.match(await settings.locator('.dashboard-update-status').textContent(), /versions cannot be compared/)
            assert.deepEqual(installations, [], 'missing version information never triggers a browser installation')
            const beforeInstallNavigation = navigations
            holdInstall = false
            await manualButton().click()
            await received(() => installations.length === 1)
            await page.waitForFunction(() => !document.querySelector('.dashboard-update-settings .button-loading'))
            assert.equal(firstStatus.current_digest, firstStatus.latest_digest, 'an explicit manual install succeeds for an unversioned archive')
            assert.equal(navigations, beforeInstallNavigation)
            assert.deepEqual(runtimeWrites, [])
            assert.deepEqual(pageErrors, [])
            return { sameVersionIgnored: true, lowerVersionIgnored: true, newerVersionAvailable: true, unversionedExplained: true, unversionedManualInstallWorks: true, automaticInstallations: 0, runtimeWrites: 0 }
        }

        mode = 'hold-check'
        await checkButton().click()
        await received(() => heldCheck)
        const canceled = page.waitForEvent('requestfailed', { predicate: request => new URL(request.url()).pathname === '/upgrade/ui' && new URL(request.url()).port === String(firstPort) })
        await page.evaluate(secondPort => { location.hash = '/settings?port=' + secondPort + '&protocol=http:' }, secondPort)
        await canceled
        await received(() => statusRequests.some(request => request.port === secondPort))
        json(heldCheck, 200, { ...firstStatus, latest_version: 'stale-first-controller' }); heldCheck = null
        await page.waitForFunction(() => document.querySelector('.dashboard-update-settings .button-loading') === null)
        assert.equal(await page.getByText('stale-first-controller', { exact: true }).count(), 0, 'late status from the first controller is discarded')
        assert.equal(await automatic().count(), 0, 'cores without scheduler metadata retain manual update support')
        assert.deepEqual(installations, [])

        mode = 'normal'
        firstStatus.auto_update = true
        firstStatus.update_interval = 8
        firstStatus.next_check = '2026-10-12T16:00:00Z'
        await page.evaluate(() => { location.hash = '/settings' })
        await received(() => statusRequests.at(-1).port === firstPort)
        await settings.getByText('8 hours', { exact: true }).waitFor()
        assert.match(await automatic().textContent(), /Enabled/)
        assert.equal(await settings.locator('time').getAttribute('datetime'), firstStatus.next_check)
        assert.deepEqual(installations, [], 'core automatic-update status never triggers a browser installation')
        await settings.getByRole('button', { name: 'Install manually', exact: true }).click()
        await received(() => heldInstall)
        assert.equal(installations.length, 1, 'an unversioned generic source can be installed explicitly despite update_available=false')
        const beforeInstallNavigation = navigations
        await openEditor()
        const draft = '# private-edit-must-survive-update\nmode: global\n'
        await text.fill(draft)
        firstStatus.current_digest = firstStatus.latest_digest
        firstStatus.update_available = false
        json(heldInstall, 200, { status: 'ok' }); heldInstall = null
        await received(() => statusRequests.at(-1).check === false)
        await page.waitForFunction(() => document.querySelector('.dashboard-update-settings .button-loading') === null)
        assert.equal(await text.inputValue(), draft)
        assert.equal(navigations, beforeInstallNavigation, 'background installation does not refresh or interrupt the draft')
        assert.deepEqual(runtimeWrites, [], 'UI installation does not save, patch or reload kernel configuration')
        const checksBeforeVisible = statusRequests.filter(request => request.check).length
        await page.evaluate(() => { document.dispatchEvent(new Event('visibilitychange')); window.dispatchEvent(new Event('online')) })
        await page.waitForTimeout(40)
        assert.equal(statusRequests.filter(request => request.check).length, checksBeforeVisible, 'browser events do not trigger checks even when core automatic updates are enabled')
        await closeEditor()

        mode = 'hold-check'
        firstStatus.latest_digest = 'c'.repeat(64)
        firstStatus.update_available = false
        await checkButton().click()
        await received(() => heldCheck)
        mode = 'normal'
        json(heldCheck, 200, firstStatus); heldCheck = null
        await page.waitForFunction(() => document.querySelector('.dashboard-update-settings .button-loading') === null)
        assert.equal(installations.length, 1, 'a manual check never installs, even when automatic updates are enabled in core YAML')
        holdInstall = false
        await settings.getByRole('button', { name: 'Install manually', exact: true }).click()
        await received(() => installations.length === 2)
        await page.waitForFunction(() => document.querySelector('.dashboard-update-settings .button-loading') === null)

        // Hold browser worker lookup to reproduce opening an editor during the
        // asynchronous explicit-refresh path, without replacing any app code.
        await page.evaluate(() => {
            window.__originalRegistration = navigator.serviceWorker.getRegistration.bind(navigator.serviceWorker)
            Object.defineProperty(navigator.serviceWorker, 'getRegistration', { configurable: true, value: () => new Promise(resolve => { window.__releaseRegistration = resolve }) })
        })
        const beforeRefreshNavigation = navigations
        await settings.getByRole('button', { name: 'Refresh dashboard', exact: true }).click()
        await page.waitForFunction(() => typeof window.__releaseRegistration === 'function')
        await openEditor()
        await text.fill('# refresh-race-draft\nmode: rule\n')
        await page.evaluate(() => window.__releaseRegistration(undefined))
        await page.waitForFunction(() => !document.querySelector('.dashboard-update-settings .button-loading'))
        assert.equal(await text.inputValue(), '# refresh-race-draft\nmode: rule\n')
        assert.equal(navigations, beforeRefreshNavigation, 'an editor opened during refresh preparation blocks the final navigation')
        await closeEditor()
        await page.evaluate(() => Object.defineProperty(navigator.serviceWorker, 'getRegistration', { configurable: true, value: window.__originalRegistration }))
        const navigated = page.waitForEvent('framenavigated', frame => frame === page.mainFrame())
        await settings.getByRole('button', { name: 'Refresh dashboard', exact: true }).click()
        await navigated
        await settings.waitFor()
        assert.deepEqual(runtimeWrites, [])
        assert.deepEqual(pageErrors, [])
        return { genericSourceManuallyInstalled: true, staleControllerCanceled: true, legacyPreferenceIgnored: true, coreScheduleReadOnly: true, noBrowserScheduledRequests: true, manualCheckNeverInstalls: true, editorSurvivesInstallation: true, editorOpenedDuringRefreshBlocksNavigation: true, explicitRefreshWorks: true, runtimeWrites: 0 }
    } finally {
        await context.close()
        heldCheck?.destroy()
        heldInstall?.destroy()
        for (const wss of sockets) { for (const socket of wss.clients) socket.terminate(); wss.close() }
        await Promise.all([new Promise(resolve => first.close(resolve)), new Promise(resolve => second.close(resolve))])
    }
}

async function main() {
    browser = await chromium.launch({
        headless: true,
        executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined,
        args: process.env.PLAYWRIGHT_CHROMIUM_ARGS ? JSON.parse(process.env.PLAYWRIGHT_CHROMIUM_ARGS) : [],
    })
    const versionOnly = process.argv.includes('--version-only')
    const migration = versionOnly ? undefined : await serviceWorkerMigration()
    const controls = process.argv.includes('--service-worker-only') ? undefined : await updaterControls(versionOnly)
    console.log(JSON.stringify({ passed: true, browser: browser.version(), migration, controls }, null, 2))
}

main().catch(error => { console.error(error.stack); process.exitCode = 1 }).finally(async () => {
    if (browser) await browser.close()
})
