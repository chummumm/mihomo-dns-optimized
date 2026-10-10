#!/usr/bin/env node
// Production-build log controls against a local, memory-only controller fixture.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const path = require('node:path')
const { WebSocket, WebSocketServer } = require('ws')
const { chromium } = require('playwright')

const dist = path.resolve(process.env.UI_DIST_DIR || path.join(__dirname, '../dist'))
const limit = 200
const marker = 'log-regression-payload-'
const config = Object.freeze({ port: 0, 'socks-port': 0, 'mixed-port': 0, 'redir-port': 0, mode: 'rule', 'log-level': 'silent', 'allow-lan': false })
const rank = { debug: 0, info: 1, warning: 2, error: 3, silent: 4 }
const requests = []
const subscriptions = []
const externalRequests = []
const pageErrors = []
const consoleErrors = []
let origin

function json(res, status, data) {
    res.writeHead(status, { 'content-type': 'application/json', 'cache-control': 'no-store' })
    res.end(JSON.stringify(data))
}

const server = http.createServer((req, res) => {
    const url = new URL(req.url, 'http://127.0.0.1')
    requests.push({ method: req.method, path: url.pathname })
    // Fail closed: this fixture accepts no config reload, PATCH, DELETE or
    // other mutation. The log selector must only reconnect its read stream.
    if (req.method !== 'GET') return json(res, 405, { message: 'read-only fixture' })
    const api = {
        '/version': { version: 'isolated-log-ui', premium: false },
        '/configs': config,
        '/connections': { connections: [], uploadTotal: 0, downloadTotal: 0 },
        '/rules': { rules: [] },
        '/providers/rules': { providers: {} },
        '/providers/proxies': { providers: {} },
        '/proxies': { proxies: {
            GLOBAL: { name: 'GLOBAL', type: 'Selector', now: 'DIRECT', all: ['DIRECT'], history: [] },
            DIRECT: { name: 'DIRECT', type: 'Direct', history: [] },
        } },
    }
    if (api[url.pathname]) return json(res, 200, api[url.pathname])
    const file = path.resolve(dist, `.${url.pathname === '/' ? '/index.html' : url.pathname}`)
    if (!file.startsWith(dist + path.sep) || !fs.existsSync(file) || !fs.statSync(file).isFile()) return json(res, 404, {})
    const types = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.png': 'image/png', '.svg': 'image/svg+xml', '.woff': 'font/woff', '.woff2': 'font/woff2', '.webmanifest': 'application/manifest+json' }
    res.writeHead(200, { 'content-type': types[path.extname(file)] || 'application/octet-stream' })
    fs.createReadStream(file).pipe(res)
})

const wss = new WebSocketServer({ server })
wss.on('connection', (socket, req) => {
    const url = new URL(req.url, 'http://127.0.0.1')
    socket.on('error', () => {})
    if (url.pathname === '/logs') {
        const level = url.searchParams.get('level') || 'info'
        assert(Object.hasOwn(rank, level), `invalid log subscription: ${level}`)
        subscriptions.push({ level, socket })
    } else if (url.pathname === '/connections') {
        socket.send(JSON.stringify({ connections: [], uploadTotal: 0, downloadTotal: 0 }))
    }
})

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))
async function until(check, label, timeout = 6000) {
    const deadline = Date.now() + timeout
    while (Date.now() < deadline) {
        if (await check()) return
        await sleep(20)
    }
    throw new Error(`Timed out: ${label}`)
}
function currentSubscription() {
    return subscriptions.findLast(entry => entry.socket.readyState === WebSocket.OPEN)
}
async function waitLevel(level) {
    if (level === 'silent') {
        await until(() => subscriptions.every(entry => entry.socket.readyState === WebSocket.CLOSED), 'Silent has no log socket')
        return
    }
    await until(() => currentSubscription()?.level === level, `WebSocket level ${level}`)
    await until(() => subscriptions.filter(entry => entry.socket.readyState === WebSocket.OPEN).length === 1, 'obsolete log socket closed')
}
async function emit(events) {
    const subscription = currentSubscription()
    // Silent disconnects instead of leaving an inert server subscription. The
    // simulated core may still produce events; there is no client to receive them.
    if (!subscription) {
        assert(subscriptions.every(entry => entry.socket.readyState === WebSocket.CLOSED), 'no stale log socket')
        return
    }
    for (const event of events) {
        if (rank[event.type] < rank[subscription.level]) continue
        await new Promise((resolve, reject) => subscription.socket.send(JSON.stringify(event), error => error ? reject(error) : resolve()))
    }
    // A pong provides a bounded transport barrier. UI rendering is awaited
    // separately below; Silent has already returned without opening a socket.
    await new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error('log WebSocket ping timed out')), 3000)
        subscription.socket.once('pong', () => { clearTimeout(timer); resolve() })
        subscription.socket.ping()
    })
}
const event = (suffix, type = 'debug') => ({ type, payload: marker + suffix })
const batch = (prefix, count) => Array.from({ length: count }, (_, index) => event(`${prefix}-${index}`))

let browser
async function main() {
    assert(fs.existsSync(path.join(dist, 'index.html')), 'build dist before running the log test')
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
    const port = server.address().port
    origin = `http://127.0.0.1:${port}`
    browser = await chromium.launch({
        headless: true,
        executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined,
        args: process.env.PLAYWRIGHT_CHROMIUM_ARGS ? JSON.parse(process.env.PLAYWRIGHT_CHROMIUM_ARGS) : [],
    })
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'en-US', serviceWorkers: 'block', reducedMotion: 'reduce' })
    await context.route('**/*', route => {
        const url = new URL(route.request().url())
        if (url.origin === origin) return route.continue()
        externalRequests.push(url.origin + url.pathname)
        return route.abort()
    })
    await context.addInitScript(({ port }) => {
        localStorage.setItem('language', JSON.stringify('en_US'))
        localStorage.setItem('externalControllers', JSON.stringify([{ hostname: '127.0.0.1', port, secret: '' }]))
        localStorage.setItem('externalControllerIndex', '0')
        localStorage.setItem('profile', JSON.stringify({ breakConnections: true, logLevel: '' }))
    }, { port })
    const page = await context.newPage()
    page.setDefaultTimeout(6000)
    page.on('pageerror', error => pageErrors.push(String(error)))
    page.on('console', message => {
        if (message.type() === 'error' || /unmounted component|synchronously unmount|ReactDOM\.render/.test(message.text())) consoleErrors.push(message.text())
    })
    await page.goto(`${origin}/#/logs`, { waitUntil: 'networkidle' })
    const select = page.getByRole('button', { name: 'Log level', exact: true })
    const menu = page.getByRole('listbox')
    const panel = page.locator('.logs-panel')
    const payloads = page.locator('.log-payload')
    const frames = () => page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
    async function choose(label, level) {
        await select.click()
        await page.getByRole('option', { name: label, exact: true }).click()
        await until(async () => (await select.textContent()).trim() === label, `selector label ${label}`)
        await waitLevel(level)
    }
    async function last(suffix) {
        await until(async () => await payloads.last().textContent().catch(() => '') === marker + suffix, `latest log ${suffix}`)
    }
    async function bounded(suffix) {
        await last(suffix)
        assert.equal(await payloads.count(), limit, 'only the latest 200 logs are mounted')
        assert.equal(await page.locator('.logs-page .header-meta .reading').textContent(), String(limit), 'displayed count matches retained rows')
    }
    async function menuOpen(reason) {
        assert.equal(await select.getAttribute('aria-expanded'), 'true', reason)
        assert.equal(await menu.isVisible(), true, reason)
    }

    await waitLevel('silent')
    assert.equal(await select.isEnabled(), true, 'global silent must not disable the panel filter')
    assert.match(await select.textContent(), /^Default/)
    assert.equal(await payloads.count(), 0)
    await choose('Debug', 'debug')
    await emit([event('debug-visible'), event('info-visible', 'info')])
    await last('info-visible')
    assert.equal(await page.getByText(marker + 'debug-visible', { exact: true }).count(), 1)
    assert.equal(await page.locator('.log-debug').count(), 1)
    assert.equal(await page.locator('.log-info').count(), 1)

    // Build a genuinely scrollable log panel while below the retention cap,
    // so incoming rows can exercise its automatic scroll without changing the
    // trigger's position. Scrolling the list must leave the selector open.
    await emit(batch('scroll', 65))
    await last('scroll-64')
    const overflow = await panel.evaluate(element => ({ scroll: element.scrollHeight, client: element.clientHeight }))
    assert(overflow.scroll > overflow.client, `log panel must scroll within its card: ${JSON.stringify(overflow)}`)
    await panel.evaluate(element => { element.scrollTop = element.scrollHeight })
    await frames()
    await select.click()
    await panel.evaluate(element => { element.scrollTop -= 80 })
    await frames()
    await menuOpen('manual log scrolling keeps the menu open')
    await panel.evaluate(element => { element.scrollTop = element.scrollHeight })
    await frames()
    const scrollBefore = await panel.evaluate(element => element.scrollTop)
    await emit(batch('auto-scroll', 10))
    await last('auto-scroll-9')
    await frames()
    const scrollAfter = await panel.evaluate(element => element.scrollTop)
    assert(scrollAfter > scrollBefore, 'incoming logs auto-scroll a panel already at its bottom')
    await menuOpen('automatic log scrolling keeps the menu open')

    // A compact embedded viewport can make even this short list scroll. Limit
    // its height for this case and generate a real list scroll event.
    await menu.evaluate(element => { element.style.maxHeight = '90px'; element.scrollTop = 60 })
    await frames()
    assert(await menu.evaluate(element => element.scrollTop > 0), 'option list actually scrolled')
    await menuOpen('scrolling the options does not dismiss their own menu')
    await page.getByRole('option', { name: 'Info', exact: true }).click()
    await page.locator('.select-list').evaluate(element => { element.style.maxHeight = '' })
    await waitLevel('info')
    await emit([event('debug-filtered'), event('new-info', 'info'), event('new-warning', 'warning')])
    await last('new-warning')
    assert.equal(await page.getByText(marker + 'debug-filtered', { exact: true }).count(), 0, 'Info excludes subsequent Debug logs')
    assert.equal(await page.getByText(marker + 'new-info', { exact: true }).count(), 1)

    await choose('Silent', 'silent')
    const retainedBeforeSilent = await payloads.allTextContents()
    await emit([event('silent-debug'), event('silent-error', 'error')])
    await frames()
    assert.deepEqual(await payloads.allTextContents(), retainedBeforeSilent, 'Silent leaves existing rows but accepts no new events')
    await choose('Default', 'silent')
    await emit([event('default-error', 'error')])
    await frames()
    assert.deepEqual(await payloads.allTextContents(), retainedBeforeSilent, 'Default follows the core global silent level')

    await page.mouse.move(1, 1)
    await select.focus()
    await select.press('ArrowDown')
    await menu.waitFor()
    await select.press('ArrowDown')
    await page.locator('.select-list .option.active').filter({ hasText: /^Debug$/ }).waitFor()
    await select.press('Enter')
    await waitLevel('debug')
    assert.match(await select.textContent(), /^Debug/)
    await emit(batch('visible-cap', 245))
    await bounded('visible-cap-244')
    assert.equal(await page.getByText(marker + 'visible-cap-0', { exact: true }).count(), 0, 'old visible logs are evicted')
    assert.equal(await payloads.first().textContent(), marker + 'visible-cap-45')

    // App keeps a small log stream buffer while the page is not mounted.
    // Re-entering must not resurrect the pre-cap display or unbounded history.
    await page.getByRole('link', { name: 'Proxies', exact: true }).click()
    await page.locator('.proxies-page').waitFor()
    await emit(batch('away-cap', 260))
    await frames()
    await page.getByRole('link', { name: 'Logs', exact: true }).click()
    await bounded('away-cap-259')
    assert.equal(await payloads.first().textContent(), marker + 'away-cap-60', 'background stream keeps only its last 200 records')

    await page.setViewportSize({ width: 320, height: 740 })
    await frames()
    await choose('Info', 'info')
    await emit([event('phone-filtered'), event('phone-visible', 'info')])
    await bounded('phone-visible')
    assert.equal(await page.getByText(marker + 'phone-filtered', { exact: true }).count(), 0)
    const buttonBox = await select.boundingBox()
    assert(buttonBox.x >= 0 && buttonBox.x + buttonBox.width <= 320, 'log selector fits a 320px phone')
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, 'logs do not overflow the mobile body')
    await select.click()
    await menuOpen('mobile click opens the menu')
    await until(async () => await menu.evaluate(element => getComputedStyle(element).opacity) === '1', 'menu fully drawn')
    if (process.env.LOGS_SCREENSHOT_PATH) await page.screenshot({ path: process.env.LOGS_SCREENSHOT_PATH })
    await page.getByRole('option', { name: 'Debug', exact: true }).click()
    await waitLevel('debug')
    await emit([event('phone-debug')])
    await bounded('phone-debug')

    const storage = await page.evaluate(() => Object.fromEntries(Object.entries(localStorage)))
    assert.equal(JSON.parse(storage.profile).logLevel, 'debug', 'the chosen filter is a persisted preference')
    assert.equal(JSON.parse(storage.profile).breakConnections, true, 'other preferences survive')
    assert(Object.keys(JSON.parse(storage.profile)).every(key => ['breakConnections', 'logLevel', 'probeURL'].includes(key)), 'profile contains only preferences')
    assert.equal(JSON.stringify(storage).includes(marker), false, 'log payloads never enter localStorage')
    assert(Object.keys(storage).every(key => ['language', 'externalControllers', 'externalControllerIndex', 'profile'].includes(key)), `unexpected persisted state: ${Object.keys(storage)}`)
    assert.deepEqual(requests.filter(request => request.method !== 'GET'), [], 'no configuration writes or other HTTP mutations')
    assert.equal(requests.some(request => /flush|clear|restart|reload|\/dns\//i.test(request.path)), false, 'no DNS clear/reload endpoint is contacted')
    assert.equal(config['log-level'], 'silent', 'the controller global log level remains untouched')
    assert.equal(subscriptions.some(entry => entry.level === 'silent'), false, 'Silent never creates an idle server subscription')
    assert.deepEqual(externalRequests, [], 'all fonts, assets and controller requests stay local')
    assert.deepEqual(pageErrors, [])
    assert.deepEqual(consoleErrors, [])
    console.log(JSON.stringify({ passed: true, browser: browser.version(), subscriptionLevels: subscriptions.map(entry => entry.level), globalLevel: config['log-level'], silentHasNoSubscription: true, retainedRows: limit, keyboard: true, mobileWidth: 320, menuSurvivesLogAndOptionScrolling: true, backgroundBufferBounded: true, mutationRequests: 0, persistedLogPayloads: 0, externalRequests, pageErrors, consoleErrors }, null, 2))
}

main().catch(error => { console.error(error.stack); process.exitCode = 1 }).finally(async () => {
    if (browser) await browser.close()
    for (const socket of wss.clients) socket.terminate()
    wss.close()
    server.close()
})
