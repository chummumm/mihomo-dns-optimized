#!/usr/bin/env node
// Runs only against an isolated local fixture server and the selected static build.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const path = require('node:path')
const { WebSocketServer } = require('ws')
const { chromium } = require('playwright')

const dist = path.resolve(process.env.UI_DIST_DIR || path.join(__dirname, '../dist'))
const config = { port: 0, 'socks-port': 0, 'mixed-port': 0, 'redir-port': 0, mode: 'rule', 'log-level': 'info', 'allow-lan': false }
function connection(index) {
    return {
        id: 'connection-' + index,
        metadata: {
            network: 'tcp', type: 'Socks5', host: 'row-' + String(index).padStart(5, '0') + '.test',
            sourceIP: '192.0.2.' + (101 + index % 2), sourcePort: String(20000 + index),
            destinationIP: '198.51.100.1', destinationPort: '443',
        },
        upload: 100, download: 200, start: new Date(Date.UTC(2026, 0, 1, 0, 0, index)).toISOString(),
        chains: ['DIRECT'], rule: 'Match', rulePayload: '',
    }
}
let connections = Array.from({ length: 400 }, (_, index) => connection(index))
const snapshot = () => ({ uploadTotal: 10000, downloadTotal: 20000, connections })
const server = http.createServer((req, res) => {
    const pathname = new URL(req.url, 'http://127.0.0.1').pathname
    const api = {
        '/version': { version: 'isolated-connection-test', premium: false }, '/configs': config,
        '/connections': snapshot(), '/rules': { rules: [] },
        '/providers/proxies': { providers: {} }, '/providers/rules': { providers: {} },
        '/proxies': { proxies: { GLOBAL: { name: 'GLOBAL', type: 'Selector', now: 'DIRECT', history: [], all: ['DIRECT'] }, DIRECT: { name: 'DIRECT', type: 'Direct', history: [] } } },
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
})
const wss = new WebSocketServer({ server })
const sockets = new Set()
wss.on('connection', (socket, req) => {
    if (new URL(req.url, 'http://127.0.0.1').pathname !== '/connections') return
    sockets.add(socket)
    socket.send(JSON.stringify(snapshot()))
    socket.on('close', () => sockets.delete(socket))
})

let browser
const pageErrors = []
const observations = []
const externalRequests = []

async function main() {
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
    const port = server.address().port
    browser = await chromium.launch({
        headless: true,
        executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined,
        args: process.env.PLAYWRIGHT_CHROMIUM_ARGS ? JSON.parse(process.env.PLAYWRIGHT_CHROMIUM_ARGS) : [],
    })
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, locale: 'en-US', serviceWorkers: 'block' })
    await context.route('**/*', route => {
        const url = new URL(route.request().url())
        if (url.hostname === '127.0.0.1' && url.port === String(port)) return route.continue()
        externalRequests.push({ url: url.origin + url.pathname, type: route.request().resourceType() })
        return route.abort()
    })
    await context.addInitScript(({ port }) => {
        localStorage.setItem('language', JSON.stringify('en_US'))
        localStorage.setItem('externalControllers', JSON.stringify([{ hostname: '127.0.0.1', port, secret: '' }]))
        localStorage.setItem('externalControllerIndex', '0')
    }, { port })
    const page = await context.newPage()
    page.setDefaultTimeout(10000)
    page.on('pageerror', error => pageErrors.push(String(error)))
    await page.goto('http://127.0.0.1:' + port + '/#/connections', { waitUntil: 'networkidle' })

    async function clickHeader(label) {
        const header = page.locator('.connections-th').filter({ hasText: new RegExp('^' + label) }).locator('div').first()
        const rect = await header.boundingBox()
        const viewport = page.viewportSize()
        const before = await header.textContent()
        assert.ok(rect && rect.x >= 0 && rect.x + rect.width <= viewport.width, label + ': header is horizontally visible')
        assert.ok(rect.y >= 0 && rect.y + rect.height <= viewport.height, label + ': header is vertically visible')
        // Locator.click scrolls sticky elements into view before clicking. A
        // user clicks the already visible header without that extra scroll.
        await page.mouse.click(rect.x + rect.width / 2, rect.y + rect.height / 2)
        await page.waitForFunction(({ label, before }) => {
            const header = [...document.querySelectorAll('.connections-th')].find(item => item.textContent.startsWith(label))
            return header !== undefined && header.firstElementChild?.textContent !== before
        }, { label, before })
    }
    const waitCount = count => page.waitForFunction(count => document.querySelector('.connections-devices-item')?.textContent.trim() === 'All (' + count + ')', count)
    const waitRows = count => page.waitForFunction(count => Number(document.querySelector('[role="table"]')?.getAttribute('aria-rowcount')) === count + 1, count)
    const waitTableSize = () => page.waitForFunction(() => {
        const viewport = document.querySelector('.connections-viewport')
        const scroll = document.querySelector('.connections-scroll')
        // AutoSizer commits after the controls' layout changes. Wait for that
        // commit, then independently assert the scrolling and visible rows.
        return viewport && scroll && scroll.offsetHeight === viewport.clientHeight && scroll.offsetWidth === viewport.clientWidth
    })
    let expectedHorizontal = 0
    async function send(next = connections, rate) {
        connections = next
        if (rate) connections = connections.map(item => ({ ...item, download: item.download + rate(item) }))
        for (const socket of sockets) socket.send(JSON.stringify(snapshot()))
        await page.waitForTimeout(70)
    }
    async function position(label) {
        const result = await page.evaluate(label => {
            const scroll = document.querySelector('.connections-scroll')
            const rect = scroll.getBoundingClientRect()
            const rows = [...document.querySelectorAll('.connections-row')]
            const visible = rows.filter(row => row.getBoundingClientRect().bottom > rect.top + 30 && row.getBoundingClientRect().top < rect.top + scroll.clientHeight)
            const first = visible[0]
            const last = visible[visible.length - 1]
            return {
                label,
                top: scroll.scrollTop, left: scroll.scrollLeft,
                maximum: Math.max(0, scroll.scrollHeight - scroll.clientHeight),
                first: first?.dataset.connectionId, last: last?.dataset.connectionId,
                rendered: rows.length,
                tableRows: Number(document.querySelector('[role="table"]').getAttribute('aria-rowcount')) - 1,
                devices: [...document.querySelectorAll('.connections-devices-item')].map(item => item.textContent.trim()),
                stickyLeft: first?.querySelector('.fixed')?.getBoundingClientRect().left,
                scrollLeft: rect.left,
                lastBottom: last?.getBoundingClientRect().bottom,
                viewportBottom: rect.top + scroll.clientHeight,
                screenBottom: window.innerHeight,
                width: scroll.clientWidth, height: scroll.clientHeight,
                devicesHeight: document.querySelector('.connections-devices-item').parentElement.clientHeight,
                historyNoteHeight: document.querySelector('.connections-history-note')?.clientHeight ?? 0,
            }
        }, label)
        observations.push(result)
        assert.ok(result.rendered < 70, 'virtual rows must remain bounded')
        return result
    }
    async function bottom(label) {
        await page.locator('.connections-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; element.scrollLeft = 320 })
        await page.waitForTimeout(80)
        const result = await position(label)
        expectedHorizontal = result.left
        assert.ok(Math.abs(result.maximum - result.top) <= 2, label + ': bottom position')
        assert.ok(Math.abs(result.stickyLeft - result.scrollLeft) <= 1, label + ': sticky first column')
        assert.ok(result.lastBottom <= result.viewportBottom + 1, label + ': final row is fully visible')
        assert.ok(result.viewportBottom <= result.screenBottom + 1, label + ': table fits the visible page')
        return result
    }
    function stillBottom(result, label) {
        assert.ok(Math.abs(result.maximum - result.top) <= 2, label + ': must remain at bottom')
        assert.ok(Math.abs(result.stickyLeft - result.scrollLeft) <= 1, label + ': sticky first column')
        assert.equal(result.left, expectedHorizontal, label + ': horizontal offset must survive refresh')
        assert.ok(result.lastBottom <= result.viewportBottom + 1, label + ': final row is fully visible')
        assert.ok(result.viewportBottom <= result.screenBottom + 1, label + ': table fits the visible page')
    }

    await waitCount(400); await waitRows(400)
    assert.deepEqual((await position('all_400')).devices, ['All (400)', '192.0.2.101 (200)', '192.0.2.102 (200)'])
    await page.locator('.connections-devices-item').filter({ hasText: '192.0.2.101' }).click()
    await waitRows(200)
    await page.locator('.connections-devices-item').first().click()
    await waitRows(400)
    await page.getByText('Keep closed connections', { exact: true }).click()
    const initialBottom = await bottom('initial_bottom')
    assert.equal(initialBottom.last, 'connection-0')
    for (let tick = 0; tick < 8; tick++) {
        await send(connections, item => Number(item.id.slice(11)) + tick + 1)
        stillBottom(await position('bottom_refresh_' + tick), 'refresh ' + tick)
    }
    await send(connections.concat(Array.from({ length: 10 }, (_, index) => connection(400 + index))))
    await waitCount(410)
    stillBottom(await position('bottom_new_connections'), 'append newest connections')
    await send(connections.filter(item => Number(item.id.slice(11)) < 330))
    await waitCount(410)
    stillBottom(await position('bottom_closed_retained'), 'retain closed connections')

    await clickHeader('Speed')
    await send(connections, item => Number(item.id.slice(11)) + 1)
    await page.locator('.connections-scroll').evaluate(element => { element.scrollTop = 4500 })
    await page.waitForTimeout(80)
    const reading = await position('reading_before_rate_change')
    await send(connections, item => 330 - Number(item.id.slice(11)))
    const readingAfter = await position('reading_after_rate_change')
    assert.equal(readingAfter.first, reading.first, 'dynamic sort preserves the reading anchor')
    for (const label of ['Host', 'Time', 'Download', 'Speed', 'Time', 'Host']) {
        await bottom('before_sort_' + label)
        await clickHeader(label)
        await send(connections, item => Number(item.id.slice(11)) % 9 + 1)
        const result = await position('after_sort_' + label)
        assert.ok(Math.abs(result.maximum - result.top) <= 2, 'rapid sorting keeps bottom: ' + label)
    }

    connections = Array.from({ length: 6100 }, (_, index) => connection(10000 + index))
    await page.reload({ waitUntil: 'networkidle' })
    await waitCount(6100); await waitRows(6100)
    await page.getByText('Keep closed connections', { exact: true }).click()
    await bottom('large_active_collection')
    await send(null)
    await waitCount(5000); await waitRows(5000)
    await page.getByText(/Oldest records removed: 1100/).waitFor()
    stillBottom(await position('history_limit'), 'history eviction')
    await send([connection(20000), connection(20001), connection(20002)])
    await waitCount(5003)
    const wide = await position('history_plus_active')
    stillBottom(wide, 'active connections are not capped')
    await page.setViewportSize({ width: 600, height: 900 })
    await waitTableSize()
    const narrow = await position('narrow_after_resize')
    stillBottom(narrow, 'narrow viewport resize')
    assert.ok(narrow.historyNoteHeight > wide.historyNoteHeight, 'history note wraps in narrow viewport')
    const beforeNarrow = connections
    // Keep the two retained IPv4 groups and add three long IPv6 device groups
    // so this fixture wraps with the different fonts used by local and CI browsers.
    await send(connections.map((item, index) => ({
        ...item,
        metadata: { ...item.metadata, sourceIP: `2001:db8:1234:5678:9abc:def0:1234:111${index + 1}` },
    })))
    await page.waitForFunction(() => document.querySelectorAll('.connections-devices-item').length === 6)
    await waitTableSize()
    const wrapped = await position('narrow_device_wrap')
    stillBottom(wrapped, 'device wrapping during refresh')
    assert.ok(wrapped.devicesHeight > narrow.devicesHeight, 'device controls wrap to another line')
    await send(beforeNarrow)
    await page.waitForFunction(() => document.querySelectorAll('.connections-devices-item').length === 3)
    await waitTableSize()
    stillBottom(await position('narrow_device_unwrap'), 'device unwrapping during refresh')
    await page.setViewportSize({ width: 1440, height: 900 })
    await waitTableSize()
    stillBottom(await position('wide_after_resize'), 'restore wide viewport')
    await page.getByText('Clear closed records', { exact: true }).click()
    await waitCount(3); await waitRows(3)
    await page.getByText('Keep closed connections', { exact: true }).click()
    await page.locator('.connections-devices-item').filter({ hasText: '192.0.2.101' }).click()
    await waitRows(2)
    await send([connection(20001)])
    await waitCount(1); await waitRows(1)
    assert.ok(await page.locator('.connections-devices-item').first().evaluate(element => element.classList.contains('selected')), 'expired device filter resets to All')
    await send(null)
    await waitCount(0); await waitRows(0)
    await send([connection(30000), connection(30001)])
    await waitCount(2)
    await send([connection(30000)])
    await waitCount(1)

    const host = connection(31000)
    host.metadata.host = 'explicit.example.test'; host.metadata.sniffHost = 'ignored.example.test'
    const sniff = connection(31001)
    sniff.metadata.host = ''; sniff.metadata.sniffHost = 'sniff.example.test'
    const ip = connection(31002)
    ip.metadata.host = ''; ip.metadata.destinationIP = '198.51.100.123'
    await send([host, sniff, ip])
    await waitCount(3); await waitRows(3)
    for (const [id, expected] of [[host.id, 'explicit.example.test:443'], [sniff.id, 'sniff.example.test:443'], [ip.id, '198.51.100.123:443']]) {
        assert.equal((await page.locator('[data-connection-id="' + id + '"] .connections-block').first().textContent()).trim(), expected)
    }
    if (process.env.CONNECTIONS_SCREENSHOT) await page.screenshot({ path: process.env.CONNECTIONS_SCREENSHOT, fullPage: true })
    await page.getByText('Keep closed connections', { exact: true }).click()
    await page.locator('[data-connection-id="' + sniff.id + '"] .connections-block').first().click()
    const details = page.locator('.connections-card .card.translate-x-0')
    // The empty drawer also says Open. Wait for the selected connection's
    // effect to populate the opened drawer before checking its contents.
    await details.getByText(sniff.id, { exact: true }).waitFor()
    await details.getByText('sniff.example.test:443', { exact: true }).waitFor()
    await details.getByText('Open', { exact: true }).waitFor()
    assert.equal(await page.getByText('sniff.example.test:443', { exact: true }).count(), 2, 'details use the same sniffed host as the list')
    const closeConnection = details.getByRole('button', { name: 'Close', exact: true })
    assert.equal(await closeConnection.isDisabled(), false)
    await send([host, ip])
    await waitCount(3)
    await details.getByText('Closed', { exact: true }).waitFor()
    assert.equal(await closeConnection.isDisabled(), true, 'retained details reflect closure on the next snapshot')

    await details.locator('.icon-close').click()
    await page.getByText('Keep closed connections', { exact: true }).click()
    const dns = connection(32000)
    dns.dns = true
    dns.metadata.destinationPort = '853'
    dns.chains = ['dns-leaf', 'dns-inner-group', 'dns-outer-group']
    dns.rule = 'DomainSuffix'; dns.rulePayload = 'dns.example.test'
    const legacyDNS = connection(32001)
    legacyDNS.metadata.type = 'Inner'; legacyDNS.metadata.inboundName = 'DNS-TRANSPORT'
    legacyDNS.chains = ['legacy-dns-leaf', 'legacy-dns-group']
    const ordinary53 = connection(32002)
    ordinary53.metadata.destinationPort = '53'; ordinary53.metadata.inboundName = 'DNS'
    ordinary53.chains = ['ordinary-leaf', 'ordinary-inner-group', 'ordinary-outer-group']
    ordinary53.rule = 'DomainSuffix'; ordinary53.rulePayload = 'ordinary.example.test'
    await send([dns, legacyDNS, ordinary53])
    await waitCount(3); await waitRows(3)
    for (const [item, type, chain, rule] of [
        [dns, 'DNS', 'dns-leaf', ''],
        [legacyDNS, 'DNS', 'legacy-dns-leaf', ''],
        [ordinary53, 'Socks5', 'ordinary-outer-group / ordinary-inner-group / ordinary-leaf', 'DomainSuffix :: ordinary.example.test'],
    ]) {
        const cells = page.locator('[data-connection-id="' + item.id + '"] .connections-block')
        assert.equal((await cells.nth(2).textContent()).trim(), type, 'connection type: ' + item.id)
        assert.equal((await cells.nth(3).textContent()).trim(), chain, 'connection node: ' + item.id)
        assert.equal((await cells.nth(4).textContent()).trim(), rule, 'connection rule: ' + item.id)
        await cells.first().click()
        await details.getByText(item.id, { exact: true }).waitFor()
        await details.getByText(type, { exact: true }).waitFor()
        await details.getByText(chain, { exact: true }).waitFor()
        if (type === 'DNS') {
            assert.equal(await details.getByText('Rule', { exact: true }).count(), 0, 'DNS details omit matched rule')
            assert.ok(!(await details.textContent()).includes('group'), 'DNS details omit proxy groups')
        } else {
            await details.getByText(rule, { exact: true }).waitFor()
            assert.equal(await details.getByText('Rule', { exact: true }).count(), 1, 'ordinary port 53 keeps its rule')
        }
        await details.locator('.icon-close').click()
    }
    observations.push({ label: 'dns_connection_presentation', markedDNS: 'DNS / dns-leaf', legacyDNS: 'DNS / legacy-dns-leaf', ordinaryPort53: 'Socks5 with unchanged chain and rule' })
    assert.deepEqual(pageErrors, [])
    assert.ok(externalRequests.every(request => request.type === 'font' && request.url === 'http://at.alicdn.com/t/font_841708_ok9czskbhel.ttf'), 'only the inherited icon font may attempt an external request; all external requests are blocked')
    console.log(JSON.stringify({ passed: true, browser: browser.version(), observations, pageErrors, blockedExternalRequests: externalRequests }, null, 2))
}

main().catch(error => {
    console.error(error.stack)
    console.error(JSON.stringify({ lastObservations: observations.slice(-3), pageErrors }, null, 2))
    process.exitCode = 1
}).finally(async () => {
    if (browser) await browser.close()
    for (const socket of wss.clients) socket.terminate()
    wss.close(); server.close()
})
