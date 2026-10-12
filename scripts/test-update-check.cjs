#!/usr/bin/env node
// Exercise the unified controller-owned update task. All servers and releases
// are fixtures; no browser request is allowed outside these two controllers.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const path = require('node:path')
const { chromium } = require('playwright')
const { WebSocketServer } = require('ws')

const dist = path.resolve(process.env.UI_DIST_DIR || path.join(__dirname, '../dist'))
const uiVersion = require('../package.json').version
const installedUI = '0.99.0'
const requests = []
const taskRequests = []
const outsideRequests = []
const errors = []
const held = new Set()
const aborted = []
const saves = []
const reloads = []
const browserPOSTs = []
let browser
let navigations = 0
let origin = ''

function makeController (name, version) {
    const current = {
        name, secret: `fixture-${name}-secret`, version, uiVersion, generation: 1, serial: 0,
        getStatus: 200, getNetworkError: false, holdGET: false, postMode: 'normal', accepted: new Map(),
        file: { path: `/fixture/${name}/config.yaml`, content: 'mode: rule\n# retained-editor-fixture\n', revision: 'revision-1', max_size: 4 * 1024 * 1024, writable: true },
    }
    current.snapshot = idle(current)
    return current
}

function items (current) {
    return [
        { id: 'kernel', kind: 'kernel', name: 'mihomo-optimized', status: 'pending', current_version: current.version, auto_update: false, update_interval: 6, interval_unit: 'hours' },
        { id: 'ui', kind: 'ui', name: 'mihomo', status: 'pending', current_version: current.uiVersion, auto_update: true, update_interval: 12, interval_unit: 'hours', ...schedule(7200000) },
        { id: 'geodata:geoip', kind: 'geodata', name: 'geoip.dat', status: 'pending', auto_update: true, update_interval: 48, interval_unit: 'hours', ...schedule(172800000) },
        { id: 'geodata:geosite', kind: 'geodata', name: 'geosite.dat', status: 'pending', auto_update: true, update_interval: 48, interval_unit: 'hours', ...schedule(172800000) },
        { id: 'rule-provider:remote', kind: 'rule-provider', name: `${current.name}-remote-rules`, status: 'pending', auto_update: true, update_interval: 3600, interval_unit: 'seconds', ...schedule(3600000) },
        { id: 'rule-provider:local', kind: 'rule-provider', name: `${current.name}-local-rules`, status: 'skipped', reason: 'local_provider', auto_update: false },
    ]
}

// Deliberately wrong device calendar time. The actual timer delay must be
// mapped to the browser clock; the policy interval is not its remaining delay.
function schedule (remaining) {
    return { next_check: '1970-01-02T00:00:00Z', next_check_in_ms: remaining, auto_update_state: 'scheduled' }
}

function idle (current) {
    return { instance_id: instanceID(current), state: 'idle', items: items(current) }
}

const instanceID = current => Buffer.from(current.name).toString('hex') + current.generation.toString(16).padStart(8, '0')

function startTask (current, requestId) {
    current.snapshot = {
        instance_id: instanceID(current), task_id: `${current.name}-task-${++current.serial}`,
        request_id: requestId, state: 'running', started_at: '2026-10-12T02:00:00Z', items: items(current),
    }
    current.accepted.set(requestId, structuredClone(current.snapshot))
}

function report (current, state, changes) {
    current.snapshot = {
        ...current.snapshot, state,
        finished_at: state === 'completed' || state === 'partial' ? '2026-10-12T02:00:30Z' : undefined,
        items: current.snapshot.items.map(item => ({ ...item, ...changes[item.id] })),
    }
}

function restart (current, version) {
    current.generation++
    current.version = version
    current.accepted.clear()
    current.snapshot = idle(current)
}

function truncate (res) {
    // Having received headers, Chromium reports a failed response without
    // replaying the POST. This isolates the application's GET-only recovery.
    res.writeHead(202, { 'content-type': 'application/json', 'content-length': '8192' })
    res.write('{"instance_id":')
    setTimeout(() => res.destroy(), 10)
}

function json (res, status, body) {
    res.writeHead(status, { 'content-type': 'application/json', 'cache-control': 'no-store' })
    res.end(JSON.stringify(body))
}

function hold (res, current, method, body, status) {
    const pending = { res, current, method, release: () => json(res, status, body) }
    held.add(pending)
    res.on('close', () => {
        held.delete(pending)
        if (!res.writableEnded) aborted.push({ controller: current.name, method })
    })
}

function handle (req, res, current) {
    const url = new URL(req.url, 'http://127.0.0.1')
    requests.push({ controller: current.name, path: url.pathname, method: req.method })
    res.setHeader('Access-Control-Allow-Origin', origin)
    res.setHeader('Access-Control-Allow-Headers', 'Authorization, Content-Type, Idempotency-Key, X-Mihomo-Instance-ID')
    res.setHeader('Access-Control-Allow-Methods', 'GET, POST, PUT, PATCH, OPTIONS')
    if (req.method === 'OPTIONS') { res.writeHead(204); res.end(); return }
    if (url.pathname === '/upgrade' || url.pathname.startsWith('/upgrade/')) {
        if (url.pathname !== '/upgrade/check') { taskRequests.push({ controller: current.name, path: url.pathname, method: req.method }); json(res, 404, {}); return }
        if (req.headers.authorization !== `Bearer ${current.secret}`) { json(res, 401, { message: 'unauthorized' }); return }
        const request = { controller: current.name, path: url.pathname, method: req.method, query: url.search, authorization: req.headers.authorization, requestId: req.headers['idempotency-key'], instanceId: req.headers['x-mihomo-instance-id'] }
        taskRequests.push(request)
        if (req.method === 'GET') {
            if (current.getNetworkError) { res.destroy(); return }
            if (current.getStatus !== 200) { json(res, current.getStatus, { message: current.getStatus === 401 ? 'unauthorized' : 'unsupported' }); return }
            const snapshot = structuredClone(current.snapshot)
            if (current.holdGET) hold(res, current, 'GET', snapshot, 200)
            else json(res, 200, snapshot)
            return
        }
        if (req.method === 'POST') {
            let body = ''
            req.on('data', chunk => { body += chunk })
            req.on('end', () => {
                request.body = body
                if (body !== '' || url.search !== '') { json(res, 400, { message: 'invalid_request' }); return }
                if (!/^[0-9a-f]{32}$/.test(request.requestId || '') || !/^[0-9a-f]{32}$/.test(request.instanceId || '')) { json(res, 400, { message: 'invalid_request' }); return }
                if (request.instanceId !== current.snapshot.instance_id) { json(res, 409, { message: 'instance_changed' }); return }
                const previous = current.accepted.get(request.requestId)
                if (previous) {
                    if (previous.task_id !== current.snapshot.task_id) { json(res, 409, { message: 'request_already_seen' }); return }
                    // A zero-byte POST can be replayed by the browser's network
                    // stack after an empty response. Its identity must survive.
                    if (current.postMode === 'lose') res.destroy()
                    else json(res, 202, current.snapshot)
                    return
                }
                if (current.postMode === 'busy') { json(res, 409, { message: 'busy' }); return }
                if (current.postMode === 'lose-unaccepted') { res.destroy(); return }
                if (current.postMode === 'lose-after-restart') { restart(current, 'mihomo-optimized-v9.9.9'); res.destroy(); return }
                if (current.postMode === 'lose-other-task') {
                    startTask(current, 'f'.repeat(32))
                    current.postMode = 'lose-unaccepted'
                    res.destroy()
                    return
                }
                startTask(current, request.requestId)
                if (current.postMode === 'lose') res.destroy()
                else if (current.postMode === 'lose-truncated') truncate(res)
                else if (current.postMode === 'hold') hold(res, current, 'POST', structuredClone(current.snapshot), 202)
                else json(res, 202, current.snapshot)
            })
            return
        }
        json(res, 405, {}); return
    }
    if (url.pathname === '/configs/file' && req.method === 'PUT') {
        let body = ''
        req.on('data', chunk => { body += chunk })
        req.on('end', () => {
            const payload = JSON.parse(body)
            if (payload.revision !== current.file.revision) { json(res, 409, { message: 'revision conflict' }); return }
            saves.push({ controller: current.name, ...payload })
            current.file.content = payload.content
            current.file.revision = `revision-${saves.length + 1}`
            json(res, 200, current.file)
        })
        return
    }
    if (url.pathname === '/configs' && req.method !== 'GET') {
        reloads.push({ controller: current.name, method: req.method })
        res.writeHead(204); res.end(); return
    }
    const api = {
        '/version': { version: current.version, premium: false },
        '/configs': { port: 7890, 'socks-port': 7891, 'mixed-port': 0, 'redir-port': 0, mode: 'rule', 'log-level': 'info', 'allow-lan': false },
        '/configs/file': current.file,
        '/connections': { connections: [], uploadTotal: 0, downloadTotal: 0 },
        '/dns/observability': { version: 1, storage: 'memory', instance_id: current.name, enabled: false, memory_limit_bytes: 8 * 1024 * 1024, accounted_bytes: 0 },
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

const a = makeController('controller-a', 'mihomo-optimized-v1.0.3')
const b = makeController('controller-b', 'mihomo-optimized-v9.0.0')
const first = http.createServer((req, res) => handle(req, res, a))
const second = http.createServer((req, res) => handle(req, res, b))
const sockets = [new WebSocketServer({ server: first }), new WebSocketServer({ server: second })]
for (const wss of sockets) wss.on('connection', (socket, req) => {
    const pathname = new URL(req.url, 'http://127.0.0.1').pathname
    socket.send(JSON.stringify(pathname === '/logs' ? { type: 'info', payload: 'retained-log-fixture' } : pathname === '/traffic' ? { up: 0, down: 0 } : { connections: [], uploadTotal: 0, downloadTotal: 0 }))
})

const sleep = delay => new Promise(resolve => setTimeout(resolve, delay))
async function until (predicate, label, timeout = 10000) {
    const deadline = Date.now() + timeout
    while (Date.now() < deadline) {
        if (await predicate()) return
        await sleep(25)
    }
    throw new Error(`Timed out: ${label}`)
}
const wirePOSTs = current => taskRequests.filter(request => request.method === 'POST' && (!current || request.controller === current.name))
const posts = current => [...new Map(wirePOSTs(current).map(request => [request.requestId, request])).values()]
const gets = current => taskRequests.filter(request => request.method === 'GET' && request.controller === current.name)

async function main () {
    await Promise.all([first, second].map(server => new Promise(resolve => server.listen(0, '127.0.0.1', resolve))))
    a.port = first.address().port
    b.port = second.address().port
    origin = `http://127.0.0.1:${a.port}`
    browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined, args: process.env.PLAYWRIGHT_CHROMIUM_ARGS ? JSON.parse(process.env.PLAYWRIGHT_CHROMIUM_ARGS) : ['--no-sandbox', '--disable-dev-shm-usage'] })
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'en-US', serviceWorkers: 'block' })
    await context.route('**/*', route => {
        const url = new URL(route.request().url())
        if (url.hostname === '127.0.0.1' && [String(a.port), String(b.port)].includes(url.port)) return route.continue()
        outsideRequests.push({ url: url.href, headers: route.request().headers() })
        return route.abort()
    })
    await context.addInitScript(({ port, secret }) => {
        localStorage.setItem('language', JSON.stringify('en_US'))
        localStorage.setItem('externalControllers', JSON.stringify([{ hostname: '127.0.0.1', port, secret }]))
        localStorage.setItem('externalControllerIndex', '0')
        localStorage.setItem('profile', JSON.stringify({ autoUpdateDashboard: true, autoUpdateCore: true }))
    }, { port: a.port, secret: a.secret })
    const page = await context.newPage()
    // Use one CDP session for both the initial zone and later changes. A
    // context-level timezoneId would reserve the override in Playwright's own
    // session and make a second session's override fail before the assertion.
    const devtools = await context.newCDPSession(page)
    await devtools.send('Emulation.setTimezoneOverride', { timezoneId: 'Asia/Shanghai' })
    page.setDefaultTimeout(10000)
    page.on('pageerror', reason => errors.push(String(reason)))
    page.on('request', request => {
        if (request.method() === 'POST' && new URL(request.url()).pathname === '/upgrade/check') {
            browserPOSTs.push({ requestId: request.headers()['idempotency-key'], instanceId: request.headers()['x-mihomo-instance-id'] })
        }
    })
    page.on('framenavigated', frame => { if (frame === page.mainFrame()) navigations++ })
    await page.clock.install({ time: new Date('2026-10-12T15:58:00Z') })
    const hash = (current, route = 'settings') => `/${route}?host=127.0.0.1&port=${current.port}&secret=${encodeURIComponent(current.secret)}&protocol=http:`
    const navigate = async (current, route) => page.evaluate(value => { location.hash = value }, hash(current, route))
    const updates = page.locator('.settings-update')
    const row = id => updates.locator(`[data-update-id="${id}"]`)
    const status = id => row(id).getAttribute('data-update-status')
    const summary = () => updates.locator('.settings-update-summary').textContent()
    const action = () => updates.locator('.settings-update-footer > button')
    const editor = page.locator('.config-editor-content')
    const text = editor.locator('textarea')
    const poll = async (predicate, label, delay = 1700) => {
        await page.clock.fastForward(delay)
        await until(predicate, label)
    }
    async function settled (current) {
        await until(async () => await row('kernel').locator('.settings-update-version').first().textContent().catch(() => '') === current.version && !(await action().isDisabled()) && (await action().textContent()).includes('Check and update'), `${current.name} status loaded`)
    }
    async function noPolicyControls () {
        assert.equal(await updates.locator('input, select, [role="switch"]').count(), 0, 'YAML update policy is not editable in settings')
        assert.equal(await page.locator('input').evaluateAll(inputs => inputs.filter(input => /external-ui|dashboard-(path|url)/i.test([input.name, input.id, input.placeholder].join(' '))).length), 0, 'no panel URL or path inputs return')
    }

    await page.goto(`${origin}/ui/#${hash(a)}`, { waitUntil: 'networkidle' })
    await settled(a)
    assert.equal(await page.title(), 'mihomo')
    assert.equal(await page.locator('.sidebar-name').textContent(), 'mihomo')
    assert.equal(await action().count(), 1)
    await noPolicyControls()
    assert.equal(posts().length, 0, 'mount only reads the current task')
    assert.match(await row('kernel').textContent(), /Automatic updates off/)
    assert.match(await row('ui').textContent(), /every 12 h/)
    assert.match(await row('geodata:geoip').textContent(), /every 48 h/)
    assert.match(await row('rule-provider:remote').textContent(), /every 3600 s/)
    assert.match(await row('rule-provider:local').textContent(), /Local provider/)

    const nextTime = id => row(id).locator('.settings-update-schedule time')
    assert.equal(await nextTime('kernel').count(), 0, 'disabled core schedule has no invented time')
    assert.equal(await nextTime('rule-provider:local').count(), 0, 'local rules have no automatic download time')
    assert.match(await nextTime('ui').textContent(), /10\/13\/2026/, 'Shanghai display crosses midnight using the browser calendar')
    assert.match(await nextTime('ui').textContent(), /GMT\+8/)
    assert.equal(await updates.locator('details > summary time').count(), 2, 'both collapsed resource classes expose their next check')
    const corePolicy = a.snapshot.items.find(item => item.id === 'kernel')
    Object.assign(corePolicy, { auto_update: true, ...schedule(21600000) })
    await poll(async () => await nextTime('kernel').count() === 1, 'new core automatic policy appears through read-only schedule refresh', 31000)
    for (const id of ['kernel', 'ui', 'geodata:geoip', 'geodata:geosite', 'rule-provider:remote']) {
        const due = Date.parse(await nextTime(id).getAttribute('datetime'))
        const browserNow = await page.evaluate(() => Date.now())
        const remaining = a.snapshot.items.find(item => item.id === id).next_check_in_ms
        assert(Math.abs(due - browserNow - remaining) < 10000, `${id} uses actual remaining delay and browser clock, not device 1970 or policy interval`)
    }
    assert.equal(posts().length, 0, 'schedule refresh never starts an installation')

    await devtools.send('Emulation.setTimezoneOverride', { timezoneId: 'America/Los_Angeles' })
    await poll(async () => /PDT/.test(await nextTime('ui').textContent()), 'time zone changes follow the browser default', 31000)
    assert.match(await nextTime('ui').textContent(), /10\/12\/2026/, 'the same instant is still the preceding date in Los Angeles')
    const wallClock = await page.evaluate(() => Date.now())
    await page.clock.setSystemTime(new Date(wallClock + 7 * 24 * 3600000))
    await poll(async () => /10\/19\/2026/.test(await nextTime('ui').textContent()), 'browser clock corrections also change displayed update dates', 31000)
    await page.clock.setSystemTime(new Date(wallClock))
    await devtools.send('Emulation.setTimezoneOverride', { timezoneId: 'Asia/Shanghai' })

    const providerPolicy = a.snapshot.items.find(item => item.id === 'rule-provider:remote')
    Object.assign(providerPolicy, { ...schedule(60000), auto_update_state: 'retrying' })
    await poll(async () => /Next retry/.test(await row('rule-provider:remote').textContent()), 'provider error backoff uses its real retry deadline', 31000)
    Object.assign(providerPolicy, { next_check: undefined, next_check_in_ms: undefined, auto_update_state: 'checking' })
    await poll(async () => /Automatic check in progress/.test(await row('rule-provider:remote').textContent()), 'running provider waits for completion before reporting its following timer', 31000)
    assert.equal(await nextTime('rule-provider:remote').count(), 0)
    const uiPolicy = a.snapshot.items.find(item => item.id === 'ui')
    Object.assign(uiPolicy, { next_check: undefined, next_check_in_ms: undefined, auto_update_state: undefined })
    await poll(async () => /has not provided/.test(await row('ui').textContent()), 'old core missing schedule metadata degrades explicitly', 31000)
    assert.equal(await nextTime('ui').count(), 0, 'the page cannot calculate now plus the configured 12 hours')
    Object.assign(corePolicy, { auto_update: false })
    await poll(async () => await nextTime('kernel').count() === 0, 'turning automatic updates off clears the displayed time', 31000)
    Object.assign(uiPolicy, schedule(7200000))
    Object.assign(providerPolicy, schedule(3600000))
    await poll(async () => await nextTime('ui').count() === 1, 'configured schedules recover through GET', 31000)
    console.log('PASS automatic schedules: all four classes, collapsed summaries, actual provider backoff, browser time zones/clock correction, disabled and legacy cases, GET-only refresh')

    await page.getByRole('button', { name: 'Edit configuration file', exact: true }).click()
    await until(async () => await text.inputValue().catch(() => '') === a.file.content, 'retained editor content')
    const draft = 'mode: global\n# preserved-unsaved-draft\n'
    await text.fill(draft)
    const beforeClock = navigations
    await page.clock.fastForward(49 * 60 * 60 * 1000)
    await page.evaluate(() => { document.dispatchEvent(new Event('visibilitychange')); window.dispatchEvent(new Event('offline')); window.dispatchEvent(new Event('online')) })
    await sleep(80)
    assert.equal(posts().length, 0, 'stored legacy flags, time and reconnection events cannot start an update')
    assert.equal(await text.inputValue(), draft)
    assert.equal(navigations, beforeClock, 'no updater reload interrupts the draft')
    await editor.getByRole('button', { name: 'Close', exact: true }).click()
    await editor.getByRole('button', { name: 'Keep editing', exact: true }).click()
    await text.press('Control+s')
    await editor.getByText('Configuration file saved and reloaded.', { exact: true }).waitFor()
    assert.deepEqual(saves, [{ controller: a.name, content: draft, revision: 'revision-1' }])
    assert.deepEqual(reloads, [{ controller: a.name, method: 'PUT' }])
    await editor.getByRole('button', { name: 'Close', exact: true }).click()
    await editor.waitFor({ state: 'detached' })
    for (const route of ['proxies', 'connections', 'dns', 'logs', 'rules', 'settings']) {
        await navigate(a, route)
        await sleep(50)
    }
    await settled(a)
    assert.equal(posts().length, 0, 'route changes never start update tasks')
    console.log('PASS unified update: passive GET only, retired settings inert, YAML policies and editor save/reload retained')

    a.postMode = 'hold'
    await action().evaluate(element => { element.click(); element.click() })
    await until(() => [...held].some(pending => pending.current === a && pending.method === 'POST'), 'single accepted POST is held')
    assert.equal(posts(a).length, 1, 'double click is deduplicated synchronously')
    assert.equal(posts(a)[0].body, '')
    assert.equal(posts(a)[0].query, '')
    assert(await action().isDisabled())
    for (const pending of [...held]) if (pending.current === a && pending.method === 'POST') pending.release()
    a.postMode = 'normal'
    await until(async () => (await summary()).includes('Checking'), 'accepted task shown')
    a.uiVersion = installedUI
    report(a, 'partial', {
        kernel: { status: 'up_to_date' }, ui: { status: 'updated', current_version: installedUI, latest_version: installedUI },
        'geodata:geoip': { status: 'updated' }, 'geodata:geosite': { status: 'error', error: 'timeout' },
        'rule-provider:remote': { status: 'updated' },
    })
    const beforeUIUpdate = navigations
    await poll(async () => await status('geodata:geosite') === 'error' && !(await action().isDisabled()), 'partial results')
    assert.match(await row('geodata:geosite').textContent(), /timed out/i)
    assert.equal(await status('geodata:geoip'), 'updated')
    assert.equal(await status('rule-provider:remote'), 'updated')
    assert.match(await summary(), /some.*errors|partial/i)
    assert.match(await row('ui').textContent(), new RegExp(uiVersion.replaceAll('.', '\\.')))
    assert.equal(await updates.getByRole('button', { name: 'Reload page', exact: true }).count(), 1, 'installed panel offers an explicit refresh')
    assert.equal(navigations, beforeUIUpdate, 'panel install never refreshes the page automatically')
    await page.clock.fastForward(120000)
    await sleep(60)
    assert.equal(posts(a).length, 1)
    console.log('PASS unified update: one empty POST, double-click guard, per-resource outcomes, partial failure and explicit UI refresh')

    await action().click()
    await until(() => posts(a).length === 2, 'second explicit task')
    await until(async () => await status('kernel') === 'pending' && await action().isDisabled(), 'new task acceptance observed')
    report(a, 'restarting', {
        kernel: { status: 'restarting', current_version: a.version, latest_version: 'mihomo-optimized-v1.0.4' },
        ui: { status: 'up_to_date' }, 'geodata:geoip': { status: 'updated' }, 'geodata:geosite': { status: 'up_to_date' }, 'rule-provider:remote': { status: 'updated' },
    })
    await poll(async () => await status('kernel') === 'restarting', 'target version recorded before restart')
    await page.getByRole('button', { name: 'Edit configuration file', exact: true }).click()
    await until(async () => await text.inputValue().catch(() => '') === a.file.content, 'editor open during update')
    const restartDraft = 'mode: direct\n# draft-survives-kernel-restart\n'
    await text.fill(restartDraft)
    const beforeRestart = navigations
    a.getNetworkError = true
    await poll(async () => /reconnect/i.test(await summary()), 'controller disconnect during core restart')
    restart(a, 'mihomo-optimized-v1.0.4')
    a.getNetworkError = false
    await poll(async () => /upgraded core/i.test(await summary()), 'new instance verifies the exact expected core version', 2300)
    assert.equal(await status('kernel'), 'updated')
    assert.equal(await status('geodata:geoip'), 'updated', 'pre-restart resource results are retained')
    assert.equal(await text.inputValue(), restartDraft)
    assert.equal(navigations, beforeRestart, 'core restart does not reload the browser')
    assert.equal(posts(a).length, 2, 'reconnection uses GET only')
    await text.fill(a.file.content)
    await editor.getByRole('button', { name: 'Close', exact: true }).click()
    await editor.waitFor({ state: 'detached' })

    await action().click()
    await until(() => posts(a).length === 3, 'task started before leaving settings')
    await navigate(a, 'proxies')
    await sleep(80)
    await navigate(a, 'settings')
    await until(async () => await action().isDisabled() && (await summary()).includes('Checking'), 'returning to settings resumes the running task')
    assert.equal(posts(a).length, 3)
    a.holdGET = true
    await poll(() => [...held].some(pending => pending.current === a && pending.method === 'GET'), 'old controller poll held')
    const beforeSwitchAbort = aborted.length
    await navigate(b, 'settings')
    await settled(b)
    await until(() => aborted.length > beforeSwitchAbort, 'switching controller aborts its old task poll')
    a.holdGET = false
    for (const pending of [...held]) if (pending.current === a) pending.release()
    assert.match(await updates.textContent(), /controller-b-remote-rules/)
    assert(!((await updates.textContent()).includes('controller-a-remote-rules')))
    assert.equal(posts(b).length, 0, 'controller switch only reads')
    console.log('PASS unified update: exact restart verification, preserved draft/results, task recovery across routes and controller isolation')

    b.postMode = 'lose-truncated'
    await action().click()
    await until(() => posts(b).length === 1, 'POST accepted with response lost')
    await until(async () => /Waiting for the controller to reconnect/i.test(await summary()), 'lost POST enters reconnection state')
    await poll(async () => (await summary()).includes('Checking'), 'GET recovers accepted task after lost POST', 400)
    report(b, 'completed', Object.fromEntries(b.snapshot.items.filter(item => item.status !== 'skipped').map(item => [item.id, { status: 'up_to_date' }])))
    await poll(async () => !(await action().isDisabled()) && /complete/i.test(await summary()), 'recovered task finishes')
    assert.equal(posts(b).length, 1, 'lost POST is never sent twice')
    assert.equal(wirePOSTs(b).length, 1, 'truncated response isolates GET-only recovery without transport retries')

    b.postMode = 'lose'
    await action().click()
    await until(() => posts(b).length === 2, 'an empty response can replay the same POST below JavaScript')
    await until(async () => /Waiting for the controller to reconnect/i.test(await summary()), 'empty response enters reconnection state')
    await poll(async () => (await summary()).includes('Checking'), 'GET recovers the single idempotent task', 400)
    assert.equal(b.serial, 2, 'all network replays with this action ID accept only one task')
    const replayedPost = posts(b)[1]
    const transportAttempts = wirePOSTs(b).filter(request => request.requestId === replayedPost.requestId).length
    report(b, 'completed', Object.fromEntries(b.snapshot.items.filter(item => item.status !== 'skipped').map(item => [item.id, { status: 'up_to_date' }])))
    await poll(async () => !(await action().isDisabled()) && /complete/i.test(await summary()), 'task affected by empty-response retries finishes')
    b.postMode = 'normal'
    async function replay (request) {
        return await context.request.post(`http://127.0.0.1:${b.port}/upgrade/check`, {
            headers: { Authorization: `Bearer ${b.secret}`, 'Idempotency-Key': request.requestId, 'X-Mihomo-Instance-ID': request.instanceId },
        })
    }
    const completedReplay = await replay(replayedPost)
    assert.equal(completedReplay.status(), 202)
    assert.equal((await completedReplay.json()).task_id, b.snapshot.task_id)
    assert.equal(b.serial, 2, 'a delayed replay after completion cannot start another task')
    const olderReplay = await replay(posts(b)[0])
    assert.equal(olderReplay.status(), 409)
    assert.equal((await olderReplay.json()).message, 'request_already_seen')
    assert.equal(b.serial, 2, 'an earlier task identity cannot replace the latest task')

    b.postMode = 'lose-unaccepted'
    await action().click()
    await until(() => posts(b).length === 3, 'unaccepted lost POST')
    await until(async () => /Waiting for the controller to reconnect/i.test(await summary()), 'unaccepted lost response enters reconnection state')
    await poll(async () => /could not.*recover|cannot.*confirm|unconfirmed/i.test(await summary()), 'GET cannot recover a new task', 400)
    assert.equal(await status('kernel'), 'skipped', 'old completed results are not attributed to the new request')
    assert.match(await row('kernel').textContent(), /not confirmed/i)
    assert.equal(posts(b).length, 3)

    b.postMode = 'lose-after-restart'
    const beforeRestartRead = gets(b).length
    await action().click()
    await until(() => posts(b).length === 4, 'lost POST crosses a process restart before acceptance is observed')
    await until(async () => /reconnect/i.test(await summary()) || gets(b).length > beforeRestartRead, 'lost response or rejected old-instance replay enters status recovery')
    await poll(async () => /could not.*recover|cannot.*confirm|unconfirmed/i.test(await summary()) && (await row('kernel').textContent()).includes('v9.9.9'), 'new idle instance is not falsely called a confirmed task', 400)
    assert.equal(await status('kernel'), 'skipped')
    assert.equal(posts(b).length, 4)
    const restartedReplay = await replay(replayedPost)
    assert.equal(restartedReplay.status(), 409)
    assert.equal((await restartedReplay.json()).message, 'instance_changed')
    assert.equal(b.serial, 2, 'old process replay cannot start a task after restart')

    b.postMode = 'lose-other-task'
    await action().click()
    await until(() => posts(b).length === 5, 'another terminal starts a different task after our response is lost')
    await until(async () => /Waiting for the controller to reconnect/i.test(await summary()), 'lost response enters recovery before another task is read')
    await poll(async () => /could not.*recover|cannot.*confirm|unconfirmed/i.test(await summary()), 'a different request ID is not accepted as our task', 400)
    assert.notEqual(b.snapshot.request_id, posts(b)[4].requestId)
    assert.equal(await status('kernel'), 'skipped')
    assert.match(await row('kernel').textContent(), /not confirmed/i)
    report(b, 'completed', Object.fromEntries(b.snapshot.items.filter(item => item.status !== 'skipped').map(item => [item.id, { status: 'up_to_date' }])))
    await poll(async () => !(await action().isDisabled()), 'other terminal task finishes without changing attribution')
    assert.match(await summary(), /could not.*recover|cannot.*confirm|unconfirmed/i)

    b.postMode = 'busy'
    const beforeBusyRead = gets(b).length
    const beforeBusyTask = b.serial
    await action().click()
    await until(() => posts(b).length === 6, 'automatic core update rejects a new unified task as busy')
    await until(async () => /busy|running|reconnect/i.test(await summary()) || gets(b).length > beforeBusyRead, 'busy rejection enters GET-only status recovery')
    await poll(async () => /busy/i.test(await summary()) && /not started|did not start|was not started/i.test(await summary()), 'old completed GET cannot turn a rejected start into success', 400)
    assert.equal(b.serial, beforeBusyTask, 'busy response never accepts another task')
    assert.equal(await status('kernel'), 'skipped')
    assert(!(await summary()).includes('Check and update complete'))
    assert.equal(posts(b).length, 6, 'busy recovery never replays an installation from JavaScript')
    b.postMode = 'normal'

    b.getStatus = 401
    await navigate(b, 'proxies'); await sleep(50); await navigate(b, 'settings')
    await until(async () => /denied|credential|secret/i.test(await summary()), '401 is shown as an authorization error')
    const beforeRetryPosts = posts(b).length
    b.getStatus = 200
    await action().click()
    await settled(b)
    assert.equal(posts(b).length, beforeRetryPosts, 'error recovery reads the task before enabling a new installation')
    b.getStatus = 404
    await navigate(b, 'proxies'); await sleep(50); await navigate(b, 'settings')
    await until(async () => /does not support unified/i.test(await summary()), 'older core degrades explicitly')
    assert(await action().isDisabled())
    assert.equal(posts(b).length, beforeRetryPosts)
    b.getStatus = 200
    await navigate(b, 'proxies'); await sleep(50); await navigate(b, 'settings')
    await settled(b)
    console.log(`PASS unified update: lost responses use GET only, ${transportAttempts} empty-response transport attempt(s) share one task, completed/restarted replay safety, other terminal isolation, busy rejection, 401 retry and legacy 404`)

    await page.clock.resume()
    async function layout (language) {
        for (const [width, height] of [[390, 844], [320, 640], [820, 780]]) {
            await page.setViewportSize({ width, height })
            for (const details of await updates.locator('details').all()) if (await details.getAttribute('open') === null) await details.locator('summary').click()
            await updates.locator('.settings-update-footer').scrollIntoViewIfNeeded()
            const geometry = await updates.evaluate(element => {
                const bounds = element.getBoundingClientRect()
                const button = element.querySelector('.settings-update-footer > button')
                const buttonBounds = button.getBoundingClientRect()
                const label = button.querySelector('.button-label')
                const labelBounds = label.getBoundingClientRect()
                return { left: bounds.left, right: bounds.right, viewport: innerWidth, width: element.clientWidth, scrollWidth: element.scrollWidth, labelLeft: labelBounds.left, labelRight: labelBounds.right, labelWidth: label.clientWidth, labelScrollWidth: label.scrollWidth, buttonLeft: buttonBounds.left, buttonRight: buttonBounds.right }
            })
            assert(geometry.left >= 0 && geometry.right <= geometry.viewport + 1 && geometry.scrollWidth <= geometry.width + 1, `${language} ${width}px update card fits: ${JSON.stringify(geometry)}`)
            assert(geometry.labelLeft >= geometry.buttonLeft + 1 && geometry.labelRight <= geometry.buttonRight - 1 && geometry.labelScrollWidth <= geometry.labelWidth + 1, `${language} ${width}px update action remains readable`)
            await noPolicyControls()
            if (process.env.UPDATE_SCREENSHOT_DIR) {
                fs.mkdirSync(process.env.UPDATE_SCREENSHOT_DIR, { recursive: true })
                await page.screenshot({ path: path.join(process.env.UPDATE_SCREENSHOT_DIR, `updates-${language}-${width}.png`), animations: 'disabled', timeout: 15000 })
            }
        }
    }
    await layout('en')
    await page.getByRole('button', { name: '中文', exact: true }).click()
    await until(async () => (await action().textContent()).includes('检查并更新'), 'Chinese update action')
    assert.match(await row('rule-provider:remote').textContent(), /每 3600 秒/)
    assert.match(await row('ui').textContent(), /每 12 小时/)
    await layout('zh')

    assert.equal(await page.evaluate(() => Object.values(localStorage).join('\n').includes('preserved-unsaved-draft')), false)
    assert(taskRequests.every(request => request.path === '/upgrade/check' && ['GET', 'POST'].includes(request.method)), 'no separate core/UI/GeoData install endpoint is used')
    assert(posts().every(request => request.body === '' && request.query === ''), 'resource sources and destinations never come from browser input')
    assert(browserPOSTs.every(request => /^[0-9a-f]{32}$/.test(request.requestId) && /^[0-9a-f]{32}$/.test(request.instanceId)), 'each action identifies its request and controller process')
    assert.equal(browserPOSTs.length, posts().length, 'each user action issues one POST from JavaScript; response recovery only sends GET')
    assert.equal(new Set(browserPOSTs.map(request => request.requestId)).size, browserPOSTs.length, 'distinct user actions never reuse an identity')
    assert(taskRequests.every(request => request.authorization === `Bearer fixture-${request.controller}-secret`), 'every task request authenticates only to its selected controller')
    assert.deepEqual(outsideRequests, [], 'the browser does not contact GitHub or any third-party update source')
    assert.deepEqual(errors, [], 'no uncaught page errors')
    console.log(JSON.stringify({ passed: true, simulatedPassiveHours: 49, explicitPOSTs: browserPOSTs.length, transportPOSTs: wirePOSTs().length, statusGETs: taskRequests.length - wirePOSTs().length, applicationPOSTNeverRepeated: true, transportReplaysIdempotent: true, exactRestartVerified: true, draftPreserved: true, controllerIsolation: true, uncertainResultsNotFabricated: true, languages: ['en', 'zh'], widths: [320, 390, 820], thirdPartyRequests: outsideRequests.length }, null, 2))
}

main().catch(error => { console.error(error.stack || error); process.exitCode = 1 }).finally(async () => {
    for (const pending of held) pending.res.destroy()
    if (browser) await browser.close()
    for (const wss of sockets) { for (const socket of wss.clients) socket.terminate(); wss.close() }
    await Promise.all([first, second].map(server => new Promise(resolve => server.close(resolve))))
})
