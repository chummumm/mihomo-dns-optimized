/* Real-browser regression for the built dashboard. No internet or user data. */
const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const path = require('node:path')
const { chromium } = require('playwright')
const { WebSocketServer } = require('ws')

const dist = path.resolve(__dirname, '../dist')
const marker = `dns-record-${Date.now()}.test`
const maliciousAnswer = '<img src=x onerror="window.__dnsXSS=true">'
const requests = []
const preflights = []
const aborted = []
const errors = []
const responses = []
const pendingResponses = new Set()
const baseline = { queries: 100, cache_fresh: 50, cache_stale: 20, hosts: 10, fake_ip: 4, upstream: 14, reject: 0, drop: 0, errors: 2, local: 0, elapsed_ms_avg: 12.5, elapsed_ms_p95: 32.768 }
const processCounts = { ...baseline, queries: 1000, cache_fresh: 100, cache_stale: 200 }
const zeroCounts = Object.fromEntries(Object.keys(baseline).map(key => [key, 0]))
const kernelMinute = Date.parse('2030-01-01T12:00:00Z')

function makeRecords (prefix, count) {
    return Array.from({ length: count }, (_, index) => {
        const id = count - index
        const fresh = index === 0
        return {
            id: String(id), time: new Date(kernelMinute - index * 1000).toISOString(),
            qname: `${fresh ? 'needle' : `record-${id}`}.${prefix}.${marker}`,
            client: id % 2 === 0 ? '192.0.2.1' : '192.0.2.2',
            qtype: fresh ? 'AAAA' : 'A', protocol: 'udp', source: 'dns-listener',
            outcome: fresh ? 'cache_fresh' : 'upstream', cache: fresh ? 'fresh' : 'miss',
            rcode: 'NOERROR', elapsed_ms: 4.25,
            answers: fresh
                ? [{ type: 'AAAA', value: '2001:db8::42', ttl: 64 }, { type: 'TXT', value: maliciousAnswer, ttl: 30 }]
                : [{ type: 'A', value: '192.0.2.42', ttl: 300 }],
            answers_truncated: fresh, error: '', upstream: fresh ? '' : 'https://resolver.example/dns-query',
        }
    })
}

function state (name, count) {
    return { name, instance: `${name}-1`, records: makeRecords(name, count), secret: `test-${name}-secret`, supported: true, enabled: true, emptyStats: false, capabilityFailure: false, delay: '', pending: 0 }
}

const stateA = state('controller-a', 140)
const stateB = state('controller-b', 1)
const config = { port: 0, 'socks-port': 0, 'mixed-port': 0, 'redir-port': 0, mode: 'rule', 'log-level': 'info', 'allow-lan': false }
let uiOrigin = ''

function json (res, code, data) {
    res.writeHead(code, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' })
    res.end(JSON.stringify(data))
}

function api (req, res, current) {
    const url = new URL(req.url, 'http://127.0.0.1')
    if (req.headers.origin === uiOrigin) res.setHeader('Access-Control-Allow-Origin', uiOrigin)
    if (req.method === 'OPTIONS') {
        const headers = String(req.headers['access-control-request-headers'] || '').toLowerCase().split(',').map(value => value.trim()).filter(Boolean)
        preflights.push({ path: url.pathname, headers })
        res.setHeader('Access-Control-Allow-Methods', 'GET')
        res.setHeader('Access-Control-Allow-Headers', 'Content-Type, Authorization')
        res.writeHead(204)
        res.end()
        return
    }
    if (!url.pathname.startsWith('/dns/observability')) {
        const regular = {
            '/version': { version: 'dns-browser-fixture', premium: false }, '/configs': config,
            '/connections': { uploadTotal: 0, downloadTotal: 0, connections: [] },
            '/rules': { rules: [] }, '/providers/proxies': { providers: {} }, '/providers/rules': { providers: {} },
            '/proxies': { proxies: { GLOBAL: { name: 'GLOBAL', type: 'Selector', now: 'DIRECT', history: [], all: ['DIRECT'] }, DIRECT: { name: 'DIRECT', type: 'Direct', history: [] } } },
        }
        json(res, regular[url.pathname] ? 200 : 404, regular[url.pathname] || {})
        return
    }
    requests.push({ state: current.name, path: url.pathname, method: req.method, params: Object.fromEntries(url.searchParams), headers: req.headers })
    if (req.headers.authorization !== `Bearer ${current.secret}`) return json(res, 401, { message: 'unauthorized' })
    if (!current.supported) return json(res, 404, { message: 'not found' })
    let data
    if (url.pathname === '/dns/observability') {
        if (current.capabilityFailure) return json(res, 503, { message: 'temporarily unavailable' })
        data = {
            version: 1, enabled: current.enabled, storage: 'memory', instance_id: current.instance,
            started_at: new Date(kernelMinute - 86400000).toISOString(), retention_seconds: 86400,
            capacity: 4096, memory_limit_bytes: 8388608, accounted_bytes: 2300000,
            memory_budget_kind: 'bounded_accounting', latency_percentile_kind: 'logarithmic_upper_bound',
            retained: current.records.length, evicted: 12, oldest_id: current.records.at(-1)?.id || '',
            newest_id: current.records[0]?.id || '', upstream_limit: 128,
        }
    } else if (url.pathname.endsWith('/stats')) {
        const counts = current.emptyStats ? zeroCounts : baseline
        const series = Array.from({ length: 1440 }, (_, index) => ({
            time: new Date(kernelMinute - (1439 - index) * 60000).toISOString(),
            queries: current.emptyStats ? 0 : index === 1437 ? 15 : index === 1438 ? 30 : index === 1439 ? 55 : 0,
            cache_hits: current.emptyStats ? 0 : index === 1437 ? 10 : index === 1438 ? 20 : index === 1439 ? 40 : 0,
            errors: !current.emptyStats && index === 1439 ? 2 : 0, elapsed_ms_avg: 12.5,
        }))
        data = { instance_id: current.instance, started_at: new Date(kernelMinute - 86400000).toISOString(), totals: processCounts, last_24h: counts, series, top_domains: [{ name: current.records[0]?.qname || 'empty.test', count: 7 }], top_clients: [{ name: '192.0.2.1', count: 7 }], top_scope: 'retained', retained: current.records.length, evicted: 12 }
    } else if (url.pathname.endsWith('/queries')) {
        const normal = value => value.toLowerCase().replace(/\.$/, '')
        let items = current.records.filter(record =>
            (!url.searchParams.get('cursor') || Number(record.id) < Number(url.searchParams.get('cursor'))) &&
            (!url.searchParams.get('qname') || record.qname === normal(url.searchParams.get('qname'))) &&
            (!url.searchParams.get('client') || record.client === url.searchParams.get('client')) &&
            (!url.searchParams.get('outcome') || record.outcome === url.searchParams.get('outcome')) &&
            (!url.searchParams.get('qtype') || record.qtype === url.searchParams.get('qtype')))
        const limit = Number(url.searchParams.get('limit') || 50)
        const hasMore = items.length > limit
        items = items.slice(0, limit)
        data = { items, next_cursor: hasMore ? items.at(-1).id : '', has_more: hasMore, retained: current.records.length, evicted: 12, scope: 'retained', instance_id: current.instance }
    } else if (url.pathname.endsWith('/upstreams')) {
        data = { instance_id: current.instance, scope: 'process', attempt_scope: 'resolver_exchange', limit: 128, overflowed: 0, items: [
            { id: 'server-1', address: 'https://same.example/<redacted>', attempts: 100, successes: 8, errors: 2, canceled: 80, timeouts: 1, rcode_errors: 1, elapsed_ms_avg: 10 },
            { id: 'server-2', address: 'https://same.example/<redacted>', attempts: 3, successes: 0, errors: 0, canceled: 3, timeouts: 0, rcode_errors: 0, elapsed_ms_avg: 8 },
        ] }
    } else return json(res, 404, {})
    if (current.delay && url.pathname.endsWith(current.delay)) {
        current.pending++
        pendingResponses.add(res)
        const timer = setTimeout(() => json(res, 200, data), 15000)
        res.on('close', () => {
            clearTimeout(timer)
            pendingResponses.delete(res)
            current.pending--
            if (!res.writableEnded) aborted.push(url.pathname)
        })
        return
    }
    json(res, 200, data)
}

const apiServer = http.createServer((req, res) => api(req, res, stateA))
const uiServer = http.createServer((req, res) => {
    const url = new URL(req.url, 'http://127.0.0.1')
    if (/^\/(dns\/|version$|configs$|connections$|rules$|providers\/|proxies$)/.test(url.pathname)) return api(req, res, stateB)
    const file = path.resolve(dist, `.${url.pathname === '/' ? '/index.html' : url.pathname}`)
    if (!file.startsWith(`${dist}${path.sep}`) || !fs.existsSync(file) || !fs.statSync(file).isFile()) {
        res.writeHead(404)
        res.end('not found')
        return
    }
    const types = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.png': 'image/png', '.svg': 'image/svg+xml', '.webmanifest': 'application/manifest+json' }
    res.writeHead(200, {
        'Content-Type': types[path.extname(file)] || 'application/octet-stream',
        'Content-Security-Policy': "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; font-src 'none'; img-src 'self' data:; connect-src 'self' http://127.0.0.1:* ws://127.0.0.1:*; worker-src 'self'",
    })
    fs.createReadStream(file).pipe(res)
})

const sockets = [apiServer, uiServer].map(server => {
    const wss = new WebSocketServer({ server })
    wss.on('connection', ws => ws.on('error', () => {}))
    return wss
})
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))
async function until (check, label, timeout = 6000) {
    const deadline = Date.now() + timeout
    while (Date.now() < deadline) {
        if (await check()) return
        await sleep(20)
    }
    throw new Error(`Timed out: ${label}`)
}
const count = (suffix, name = stateA.name) => requests.filter(request => request.state === name && request.path === `/dns/observability${suffix}`).length
const listen = server => new Promise(resolve => server.listen(0, '127.0.0.1', resolve))

async function run () {
    let browser
    try {
        assert(fs.existsSync(path.join(dist, 'index.html')), 'build dist before running the browser test')
        await Promise.all([listen(apiServer), listen(uiServer)])
        const apiPort = apiServer.address().port
        const uiPort = uiServer.address().port
        uiOrigin = `http://127.0.0.1:${uiPort}`
        const apiOrigin = `http://127.0.0.1:${apiPort}`
        browser = await chromium.launch({
            executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined,
            headless: true,
            args: ['--no-sandbox', '--disable-dev-shm-usage', '--use-angle=swiftshader'],
        })
        const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, locale: 'en-US' })
        await context.addInitScript(() => localStorage.setItem('language', JSON.stringify('en_US')))
        const page = await context.newPage()
        page.setDefaultTimeout(6000)
        page.on('pageerror', error => errors.push(error.message))
        page.on('response', response => {
            if (new URL(response.url()).pathname.startsWith('/dns/observability')) responses.push({ url: response.url(), status: response.status(), headers: response.headers() })
        })
        await page.clock.install({ time: new Date('2026-10-10T08:00:00Z') })
        function hash (port, secret, pathname = '/dns') { return `${pathname}?host=127.0.0.1&port=${port}&secret=${encodeURIComponent(secret)}` }
        async function navigate (port, secret, pathname) { await page.evaluate(value => { location.hash = value }, hash(port, secret, pathname)) }
        const metric = name => page.locator('.dns-metric').filter({ has: page.getByText(name, { exact: true }) }).locator('strong')
        const queryButtons = () => page.locator('.dns-query-card .dns-domain-button')

        await page.goto(`${uiOrigin}/#${hash(apiPort, stateA.secret)}`)
        await until(async () => await metric('Cache hit ratio').textContent().catch(() => '') === '70%', 'same-window 70/100 cache ratio')
        assert.match(await page.locator('.dns-metric').filter({ has: page.getByText('Cache hit ratio', { exact: true }) }).textContent(), /Fresh: 50.*Stale: 20.*Since start: 30%/)
        assert.equal(await metric('QPS').textContent(), '0.5', 'QPS uses the kernel previous minute, not browser time or current minute')
        assert.match(await page.locator('.dns-metrics').textContent(), /P95 \(approximate\)/)
        assert(preflights.some(request => request.headers.includes('authorization')), 'separate origins exercised real Authorization preflight')
        assert(preflights.every(request => request.headers.every(header => ['content-type', 'authorization'].includes(header))), 'DNS requests require only supported CORS headers')
        const denied = await page.evaluate(async ({ origin, secret }) => {
            try { await fetch(`${origin}/dns/observability`, { headers: { Authorization: `Bearer ${secret}`, 'X-Forbidden-DNS': '1' } }); return false } catch { return true }
        }, { origin: apiOrigin, secret: stateA.secret })
        assert(denied, 'negative CORS control: forbidden headers must fail; web security must remain enabled')
        stateA.emptyStats = true
        await page.getByRole('button', { name: 'Refresh', exact: true }).click()
        await until(async () => await metric('Cache hit ratio').textContent() === '—', 'zero queries renders an em dash')
        stateA.emptyStats = false
        console.log('PASS DNS overview: global windows, cache ratio, QPS, approximate P95, real CORS')

        await page.getByRole('tab', { name: 'Upstreams', exact: true }).click()
        await until(async () => await page.locator('.dns-query-card tbody tr').count() === 2, 'upstream rows')
        const upstreamRows = await page.locator('.dns-query-card tbody tr').allTextContents()
        assert.match(upstreamRows[0], /80%/)
        assert.match(upstreamRows[1], /—/)
        assert.equal(await page.getByText('https://same.example/<redacted>', { exact: true }).count(), 2, 'sanitized identical addresses retain separate IDs/rows')
        console.log('PASS DNS upstreams: canceled/in-flight excluded from success denominator and stable IDs')

        await page.getByRole('tab', { name: 'Query log', exact: true }).click()
        await until(async () => await queryButtons().count() === 50, 'default 50-row page')
        const initialNames = await queryButtons().allTextContents()
        const needle = stateA.records[0].qname
        assert.equal(initialNames[0], needle)
        await queryButtons().first().click()
        const detail = page.getByRole('dialog', { name: 'DNS query details' })
        await until(async () => await detail.isVisible(), 'query details visible')
        assert.match(await detail.textContent(), /NOERROR/)
        assert.match(await detail.textContent(), /Unknown \/ not attributed/)
        assert.match(await detail.textContent(), /Fresh hit/)
        assert.equal(await detail.locator('.dns-answer-table tbody tr').first().locator('td').last().textContent(), '64', 'answer TTL is preserved')
        assert.match(await detail.textContent(), /Only part of the answer was retained/)
        assert.equal(await detail.locator('img').count(), 0, 'DNS answer text is escaped')
        assert.equal(await page.evaluate(() => window.__dnsXSS), undefined)
        await detail.getByRole('button', { name: 'Close', exact: true }).click()
        await queryButtons().nth(1).click()
        assert.match(await page.getByRole('dialog').textContent(), /https:\/\/resolver.example\/dns-query/)
        await page.getByRole('dialog').getByRole('button', { name: 'Close', exact: true }).click()
        await page.getByRole('button', { name: 'Older', exact: true }).click()
        await until(async () => (await queryButtons().allTextContents())[0] === stateA.records[50].qname, 'cursor page 2')
        const pageTwoNames = await queryButtons().allTextContents()
        assert.equal(pageTwoNames.length, 50)
        assert(!pageTwoNames.some(name => initialNames.includes(name)), 'cursor pages do not overlap')
        assert.match(await page.locator('.dns-pagination-info').textContent(), /Page 2/)
        await page.getByRole('button', { name: 'Previous', exact: true }).click()
        await until(async () => (await queryButtons().allTextContents())[0] === needle, 'previous cursor')
        await page.getByLabel('Rows per page').selectOption('100')
        await until(async () => await queryButtons().count() === 100, '100-row page')
        await page.getByLabel('Domain', { exact: true }).fill(`${needle.toUpperCase()}.`)
        await page.getByLabel('Client', { exact: true }).fill('192.0.2.1')
        await page.getByLabel('Type', { exact: true }).fill('aaaa')
        await page.getByLabel('Result source', { exact: true }).selectOption('cache_fresh')
        await page.getByRole('button', { name: 'Apply filters', exact: true }).click()
        await until(async () => await queryButtons().count() === 1, 'indexed filter combination')
        const filteredRequest = requests.filter(request => request.path.endsWith('/queries')).at(-1)
        assert.equal(filteredRequest.params.qtype, 'AAAA')
        assert.equal(filteredRequest.params.client, '192.0.2.1')
        assert.equal(filteredRequest.params.outcome, 'cache_fresh')
        assert.equal(await queryButtons().first().textContent(), needle)
        assert(await page.getByRole('button', { name: 'Older', exact: true }).isDisabled())
        await page.getByRole('button', { name: 'Reset', exact: true }).click()
        await until(async () => await queryButtons().count() === 100, 'filters reset')
        console.log('PASS DNS queries: 50/100 rows, cursor navigation, exact filters, answers/TTL/cache/upstream details')

        let before = count('/queries')
        await page.clock.fastForward(12000)
        await sleep(60)
        assert.equal(count('/queries'), before, 'query log starts paused')
        await page.getByRole('button', { name: 'Start live refresh', exact: true }).click()
        await page.clock.fastForward(5100)
        await until(() => count('/queries') > before, 'live refresh requests a new page')
        await until(async () => !(await page.getByRole('button', { name: 'Refresh', exact: true }).isDisabled()), 'live request complete')
        await page.getByRole('button', { name: 'Pause refresh', exact: true }).click()
        before = count('/queries')
        const scroll = page.locator('.dns-query-card .dns-table-scroll')
        await scroll.evaluate(element => { element.scrollTop = 430 })
        await page.clock.fastForward(12000)
        await sleep(60)
        assert.equal(count('/queries'), before, 'pause issues no final accidental refresh')
        assert.equal(await scroll.evaluate(element => element.scrollTop), 430, 'paused reading preserves scroll')
        await page.getByRole('button', { name: 'Start live refresh', exact: true }).click()
        await page.evaluate(() => { Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' }); document.dispatchEvent(new Event('visibilitychange')) })
        const beforeHidden = requests.length
        await page.clock.fastForward(20000)
        await sleep(60)
        assert.equal(requests.length, beforeHidden, 'hidden page stops all DNS polling')
        await page.evaluate(() => { delete document.visibilityState; document.dispatchEvent(new Event('visibilitychange')) })
        await until(() => requests.length > beforeHidden, 'visible page resumes polling')
        await page.getByRole('button', { name: 'Pause refresh', exact: true }).click()
        console.log('PASS DNS refresh lifecycle: pause, stable scroll, hidden-page cancellation and resume')

        await until(async () => !(await page.getByRole('button', { name: 'Refresh', exact: true }).isDisabled()), 'ready before instance reset')
        await page.getByRole('button', { name: 'Older', exact: true }).click()
        await until(async () => await queryButtons().count() === 40, 'history page before instance change')
        await queryButtons().first().click()
        stateA.instance = 'controller-a-2'
        stateA.records = makeRecords('after-instance-reset', 7)
        await page.clock.fastForward(11000)
        await until(async () => await queryButtons().count() === 7, 'new kernel instance clears old page')
        assert.equal(await page.getByRole('dialog').count(), 0, 'instance change clears selected detail')
        assert.match(await page.locator('.dns-pagination-info').textContent(), /Page 1/)
        assert.equal(await page.getByLabel('Rows per page').inputValue(), '50')
        assert.equal(await page.getByLabel('Domain', { exact: true }).inputValue(), '')
        await page.getByRole('button', { name: 'Start live refresh', exact: true }).click()
        const staleDetailName = stateA.records[0].qname
        await queryButtons().first().click()
        assert.match(await page.getByRole('dialog').textContent(), /after-instance-reset/)
        stateA.capabilityFailure = true
        stateA.instance = 'controller-a-3'
        stateA.records = makeRecords('after-failed-capability-refresh', 3)
        await page.clock.fastForward(5100)
        await until(() => responses.some(response => new URL(response.url).pathname === '/dns/observability' && response.status === 503), 'new instance capability refresh fails')
        await until(async () => await queryButtons().count() === 0, 'new instance query page is hidden before capability recovers')
        await until(async () => await page.getByRole('dialog').count() === 0, 'new instance clears old details despite failed capability refresh')
        assert.equal(await page.getByRole('tab', { name: 'Query log', exact: true }).getAttribute('aria-selected'), 'true', 'transient capability failure keeps the query page mounted')
        assert(!((await page.locator('body').textContent()).includes(staleDetailName)), 'old instance detail is no longer visible')
        stateA.capabilityFailure = false
        await page.clock.fastForward(11000)
        await until(async () => await queryButtons().count() === 3, 'capability recovers to the new instance')
        await queryButtons().first().click()
        await navigate(uiPort, stateB.secret)
        await until(async () => await page.getByRole('tab', { name: 'Overview', exact: true }).getAttribute('aria-selected') === 'true', 'controller switch resets selected tab')
        assert.equal(await page.getByRole('dialog').count(), 0, 'controller switch clears detail')
        await page.getByRole('tab', { name: 'Query log', exact: true }).click()
        await until(async () => await queryButtons().count() === 1, 'new controller records only')
        assert.equal(await queryButtons().first().textContent(), stateB.records[0].qname)
        assert(!((await page.locator('body').textContent()).includes('after-instance-reset')))
        console.log('PASS DNS identity changes: controller/instance reset; failed capability refresh cannot retain old details')

        stateB.delay = '/queries'
        const beforeAbort = aborted.length
        await page.getByRole('button', { name: 'Refresh', exact: true }).click()
        await until(() => stateB.pending > 0, 'delayed request is in flight')
        await navigate(uiPort, stateB.secret, '/settings')
        await until(() => aborted.length > beforeAbort, 'leaving DNS aborts the pending request')
        const afterLeave = requests.length
        await page.clock.fastForward(30000)
        await sleep(60)
        assert.equal(requests.length, afterLeave, 'leaving DNS stops all DNS timers')
        stateB.delay = ''
        stateA.supported = false
        await navigate(apiPort, stateA.secret)
        await until(async () => await page.getByText('This kernel does not support DNS observability.', { exact: true }).isVisible(), 'old kernel is explicitly unsupported')
        const after404 = requests.length
        await page.clock.fastForward(60000)
        await sleep(60)
        assert.equal(requests.length, after404, '404 stops capability polling')
        console.log('PASS DNS unavailable/lifecycle: old kernel 404 stops polling and route exit aborts I/O')

        await navigate(uiPort, stateB.secret)
        await until(async () => await metric('Cache hit ratio').count() === 1, 'return to supported controller')
        await page.getByRole('tab', { name: 'Query log', exact: true }).click()
        await until(async () => await queryButtons().count() === 1, 'records visible for storage audit')
        const persisted = await page.evaluate(async needle => {
            const leaks = []
            for (const [name, storage] of [['localStorage', localStorage], ['sessionStorage', sessionStorage]]) {
                for (let index = 0; index < storage.length; index++) {
                    const key = storage.key(index)
                    if (`${key} ${storage.getItem(key)}`.includes(needle)) leaks.push(`${name}:${key}`)
                }
            }
            for (const info of await indexedDB.databases()) {
                const db = await new Promise((resolve, reject) => { const request = indexedDB.open(info.name); request.onsuccess = () => resolve(request.result); request.onerror = () => reject(request.error) })
                for (const name of db.objectStoreNames) {
                    const values = await new Promise((resolve, reject) => { const request = db.transaction(name).objectStore(name).getAll(); request.onsuccess = () => resolve(request.result); request.onerror = () => reject(request.error) })
                    if (JSON.stringify(values).includes(needle)) leaks.push(`IndexedDB:${info.name}/${name}`)
                }
                db.close()
            }
            for (const name of await caches.keys()) {
                const cache = await caches.open(name)
                for (const request of await cache.keys()) {
                    if (request.url.includes('/dns/observability')) leaks.push(`CacheStorage:${request.url}`)
                    const response = await cache.match(request)
                    if (response && (await response.text()).includes(needle)) leaks.push(`CacheStorage payload:${request.url}`)
                }
            }
            return leaks
        }, marker)
        assert.deepEqual(persisted, [], 'DNS records must not enter browser persistent stores')
        assert(responses.filter(response => response.status === 200).every(response => response.headers['cache-control'] === 'no-store'), 'all successful DNS API responses forbid HTTP caching')
        assert(requests.filter(request => request.path.startsWith('/dns/observability')).every(request => request.method === 'GET'), 'UI only reads DNS APIs')
        assert(requests.filter(request => request.headers.origin === uiOrigin).every(request => !request.headers['cache-control'] && !request.headers.pragma), 'UI does not require forbidden CORS request headers')
        assert.deepEqual(errors, [], 'no uncaught browser exceptions')
        console.log('PASS DNS storage/privacy: no records in localStorage, sessionStorage, IndexedDB or CacheStorage; HTTP no-store')
        console.log(`PASS DNS browser regression (${browser.version()}, ${requests.length} DNS GET requests, two local ports)`)
    } finally {
        for (const res of pendingResponses) res.destroy()
        if (browser) await browser.close()
        for (const wss of sockets) { for (const ws of wss.clients) ws.terminate(); wss.close() }
        await Promise.all([apiServer, uiServer].map(server => new Promise(resolve => server.close(resolve))))
    }
}

run().catch(error => { console.error(error.stack || error); process.exitCode = 1 })
