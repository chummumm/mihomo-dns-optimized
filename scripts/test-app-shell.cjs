#!/usr/bin/env node
// Shared Chromium / WebKit checks. Viewport changes are geometry tests, not a
// simulation of the browser chrome on a physical iPad.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const path = require('node:path')
const { chromium, webkit } = require('playwright')
const { WebSocketServer } = require('ws')

const root = path.resolve(__dirname, '..')
const dist = path.resolve(process.env.UI_DIST_DIR || path.join(root, 'dist'))
const expectedVersion = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8')).version
const browserName = process.env.PLAYWRIGHT_BROWSER_NAME || 'chromium'
assert.ok(['chromium', 'webkit'].includes(browserName), 'supported browser engine')
let browser
let version = 'mihomo-optimized-v1.0.3'
let offline = false
const errors = [], externalRequests = [], mutations = []
const config = { path: '/fixture/config.yaml', revision: 'shell-revision', content: 'mode: rule\n' + '# file start and editor layout\n'.repeat(120), max_size: 4 * 1024 * 1024, writable: false }

function json(res, status, value) {
    res.writeHead(status, { 'content-type': 'application/json', 'cache-control': 'no-store' })
    res.end(JSON.stringify(value))
}
const server = http.createServer((req, res) => {
    const pathname = new URL(req.url, 'http://127.0.0.1').pathname
    if (req.method !== 'GET') { mutations.push({ method: req.method, pathname }); res.writeHead(405); res.end(); return }
    if (pathname === '/version') { json(res, offline ? 503 : 200, offline ? {} : { version, premium: false }); return }
    const api = {
        '/configs/file': config,
        '/configs': { port: 7890, 'socks-port': 7891, 'mixed-port': 0, 'redir-port': 0, mode: 'rule', 'log-level': 'info', 'allow-lan': false },
        '/connections': { connections: [], uploadTotal: 0, downloadTotal: 0 },
        '/rules': { rules: [] }, '/providers/rules': { providers: {} }, '/providers/proxies': { providers: {} }, '/proxies': { proxies: {} },
    }
    if (api[pathname]) { json(res, 200, api[pathname]); return }
    const filename = path.join(dist, pathname === '/' ? 'index.html' : pathname)
    if (!filename.startsWith(dist + path.sep) || !fs.existsSync(filename) || !fs.statSync(filename).isFile()) { res.writeHead(404); res.end(); return }
    const types = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml', '.png': 'image/png', '.webmanifest': 'application/manifest+json' }
    res.writeHead(200, { 'content-type': types[path.extname(filename)] || 'application/octet-stream' })
    fs.createReadStream(filename).pipe(res)
})
const sockets = new WebSocketServer({ server })
sockets.on('connection', socket => socket.send(JSON.stringify({ connections: [], uploadTotal: 0, downloadTotal: 0 })))

async function contextFor(viewport, language = 'en_US', mobile = false) {
    const port = server.address().port, origin = 'http://127.0.0.1:' + port
    const context = await browser.newContext({ viewport, locale: language === 'zh_CN' ? 'zh-CN' : 'en-US', isMobile: mobile, hasTouch: mobile, serviceWorkers: 'block' })
    await context.route('**/*', route => {
        if (new URL(route.request().url()).origin === origin) return route.continue()
        externalRequests.push(route.request().url()); return route.abort()
    })
    await context.addInitScript(({ port, language }) => {
        localStorage.setItem('language', JSON.stringify(language))
        localStorage.setItem('externalControllers', JSON.stringify([{ hostname: '127.0.0.1', port, secret: '' }]))
        localStorage.setItem('externalControllerIndex', '0')
    }, { port, language })
    const page = await context.newPage()
    page.setDefaultTimeout(10000)
    page.on('pageerror', error => errors.push(String(error)))
    await page.goto(origin + '/#/settings', { waitUntil: 'networkidle' })
    await page.locator('.header-level-1').waitFor()
    return { context, page }
}

async function assertHeaderAndShell(page, mobile) {
    await page.waitForFunction(() => document.querySelector('.route-view')?.getAnimations().every(animation => animation.playState === 'finished'))
    const state = await page.evaluate(() => {
        const rect = selector => { const r = document.querySelector(selector).getBoundingClientRect(); return { top: r.top, bottom: r.bottom, left: r.left, right: r.right, width: r.width, height: r.height } }
        const root = document.scrollingElement
        return { shell: rect('.app'), rail: rect('.sidebar'), work: rect('.page-container'), header: rect('.header-level-1'), title: rect('.header-level-1 .header-title'), height: innerHeight, width: innerWidth, documentTop: root.scrollTop, documentHeight: root.scrollHeight, documentClient: root.clientHeight }
    })
    assert.ok(Math.abs(state.shell.top) < 1 && Math.abs(state.shell.bottom - state.height) < 1, 'shell follows the visible window bounds')
    assert.ok(state.title.top >= state.work.top - 1 && state.title.bottom <= state.work.bottom + 1, 'page title stays fully inside the visible work area')
    assert.ok(state.header.left >= state.work.left - 1 && state.header.right <= state.work.right + 1, 'page heading fits the work area')
    assert.ok(Math.abs(state.rail.bottom - state.height) < 1, 'navigation follows the same bottom edge')
    assert.ok(Math.abs(state.work.top) < 1, 'work area follows the same top edge')
    if (mobile) assert.ok(state.work.bottom <= state.rail.top + 1, 'phone navigation does not cover content')
    else assert.ok(Math.abs(state.rail.top) < 1 && Math.abs(state.work.bottom - state.rail.bottom) < 1, 'desktop and tablet columns share viewport bounds')
    assert.ok(state.documentHeight <= state.documentClient + 1, 'there is no larger document behind the internal scroll area')
    assert.equal(state.documentTop, 0)
    return state
}

async function main() {
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
    const type = browserName === 'webkit' ? webkit : chromium
    browser = await type.launch({ headless: true,
        executablePath: browserName === 'webkit' ? process.env.PLAYWRIGHT_WEBKIT_EXECUTABLE_PATH : process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH,
        args: browserName === 'chromium' && process.env.PLAYWRIGHT_CHROMIUM_ARGS ? JSON.parse(process.env.PLAYWRIGHT_CHROMIUM_ARGS) : [],
    })
    const { context, page } = await contextFor({ width: 1280, height: 900 })
    assert.equal(await page.title(), 'mihomo')
    assert.equal(await page.locator('.sidebar-brand').getAttribute('title'), 'mihomo')
    assert.equal(await page.locator('.sidebar-name').textContent(), 'mihomo')
    const manifest = await page.evaluate(async () => await (await fetch(document.querySelector('link[rel="manifest"]').href)).json())
    assert.equal(manifest.name, 'mihomo'); assert.equal(manifest.short_name, 'mihomo')
    const core = page.getByRole('group', { name: 'Core ' + version, exact: true })
    const ui = page.getByRole('group', { name: 'UI ' + expectedVersion, exact: true })
    await core.waitFor(); await ui.waitFor()
    assert.equal(await core.locator('.sidebar-version-text').textContent(), 'v1.0.3')
    assert.equal(await core.getAttribute('title'), 'Core ' + version)
    assert.equal(await ui.locator('.sidebar-version-text').textContent(), expectedVersion)
    for (const item of [core, ui]) {
        const clipped = await item.locator('.sidebar-version-text').evaluate(element => element.scrollWidth > element.clientWidth + 1)
        assert.equal(clipped, false, 'the core and dashboard numbers are visible without hover')
    }
    await assertHeaderAndShell(page, false)

    // The work area can scroll without moving navigation or the document.
    await page.setViewportSize({ width: 1024, height: 650 })
    const before = await assertHeaderAndShell(page, false)
    await page.locator('.page-container').evaluate(element => { element.scrollTop = 120 })
    const scroll = await page.evaluate(() => ({ work: document.querySelector('.page-container').scrollTop, document: document.scrollingElement.scrollTop, rail: document.querySelector('.sidebar').getBoundingClientRect().top }))
    assert.ok(scroll.work > 0, 'settings remain independently scrollable')
    assert.equal(scroll.document, 0); assert.equal(scroll.rail, before.rail.top)
    await page.evaluate(() => window.scrollTo(0, 100))
    assert.equal(await page.evaluate(() => document.scrollingElement.scrollTop), 0, 'focus or scroll requests cannot move the root behind the rail')
    await context.close()

    const tablet = await contextFor({ width: 834, height: 1194 }, 'en_US', true)
    const tabletSizes = [{ width: 834, height: 1194 }, { width: 834, height: 998 }, { width: 1194, height: 638 }, { width: 1194, height: 834 }, { width: 834, height: 998 }]
    for (const size of tabletSizes) {
        await tablet.page.setViewportSize(size)
        await tablet.page.locator('.page-container').evaluate(element => { element.scrollTop = 0 })
        await assertHeaderAndShell(tablet.page, false)
    }
    await tablet.page.setViewportSize({ width: 1024, height: 480 })
    const lastLink = tablet.page.locator('.sidebar-menu .item-link').last()
    await lastLink.scrollIntoViewIfNeeded()
    const shortRail = await tablet.page.locator('.sidebar-route').evaluate(element => ({ top: element.getBoundingClientRect().top, bottom: element.getBoundingClientRect().bottom, scroll: element.scrollTop }))
    const lastBounds = await lastLink.boundingBox()
    assert.ok(shortRail.scroll > 0 && lastBounds.y >= shortRail.top - 1 && lastBounds.y + lastBounds.height <= shortRail.bottom + 1, 'navigation stays reachable above version readouts in short windows')
    await tablet.context.close()

    const phone = await contextFor({ width: 390, height: 844 }, 'en_US', true)
    const phoneSizes = [{ width: 390, height: 844 }, { width: 390, height: 648 }, { width: 320, height: 640 }]
    for (const size of phoneSizes) {
        await phone.page.setViewportSize(size)
        await phone.page.locator('.page-container').evaluate(element => { element.scrollTop = 0 })
        await assertHeaderAndShell(phone.page, true)
        await phone.page.getByRole('button', { name: 'Edit configuration file', exact: true }).click()
        await phone.page.waitForFunction(() => {
            const area = document.querySelector('.config-editor-text'), modal = document.querySelector('.config-editor-modal')
            if (!area || area.disabled || !modal) return false
            const r = modal.getBoundingClientRect()
            return Math.abs(r.top) < 1 && Math.abs(r.left) < 1 && Math.abs(r.height - innerHeight) < 1 && Math.abs(r.width - innerWidth) < 1 && area.selectionStart === 0 && area.scrollTop === 0
        })
        assert.equal(await phone.page.locator('.config-editor-text').evaluate(element => element.readOnly), true)
        const close = phone.page.locator('.config-editor-content').getByRole('button', { name: 'Close', exact: true })
        await close.scrollIntoViewIfNeeded()
        const bounds = await close.boundingBox()
        assert.ok(bounds.y >= 0 && bounds.y + bounds.height <= size.height + 1, 'portal editor footer remains reachable outside the app shell')
        await close.click()
        await phone.page.locator('.config-editor-content').waitFor({ state: 'detached' })
    }
    await phone.context.close()

    offline = true
    const disconnected = await contextFor({ width: 1024, height: 768 })
    await disconnected.page.locator('.sidebar-version.is-offline').waitFor()
    assert.equal(await disconnected.page.locator('.sidebar-live').textContent(), 'Offline')
    assert.equal(await disconnected.page.getByRole('group', { name: 'Core —', exact: true }).locator('.sidebar-version-text').textContent(), '—')
    assert.equal(await disconnected.page.getByRole('group', { name: 'UI ' + expectedVersion, exact: true }).locator('.sidebar-version-text').textContent(), expectedVersion)
    await disconnected.context.close()
    offline = false
    const chinese = await contextFor({ width: 1024, height: 768 }, 'zh_CN')
    assert.equal(await chinese.page.getByRole('group', { name: '面板 ' + expectedVersion, exact: true }).locator('.sidebar-version-label').textContent(), '面板')
    for (const name of ['内核 ' + version, '面板 ' + expectedVersion]) {
        const item = chinese.page.getByRole('group', { name, exact: true }).locator('.sidebar-version-text')
        assert.equal(await item.evaluate(element => element.scrollWidth > element.clientWidth + 1), false, 'Chinese labels leave both version numbers fully visible')
    }
    version = 'v1.19.32-alpha-legacy'
    await chinese.page.reload({ waitUntil: 'networkidle' })
    assert.equal(await chinese.page.getByRole('group', { name: '内核 ' + version, exact: true }).locator('.sidebar-version-text').textContent(), version, 'other kernel version formats remain unchanged')
    await chinese.context.close()

    assert.deepEqual(errors, []); assert.deepEqual(externalRequests, []); assert.deepEqual(mutations, [])
    console.log(JSON.stringify({ passed: true, engine: browserName, version: expectedVersion, title: 'mihomo', separateVersions: true, offlineVersion: true, tabletSizes, phoneSizes, independentScroll: true, rootScroll: false, editorPreserved: true, errors }, null, 2))
}
main().catch(error => { console.error(error.stack); process.exitCode = 1 }).finally(async () => {
    await browser?.close()
    for (const socket of sockets.clients) socket.terminate()
    sockets.close(); server.close()
})
