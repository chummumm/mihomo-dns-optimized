#!/usr/bin/env node
// Exercise configuration editing against two isolated controller fixtures.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const path = require('node:path')
const { chromium } = require('playwright')
const { WebSocketServer } = require('ws')

const dist = path.resolve(process.env.UI_DIST_DIR || path.join(__dirname, '../dist'))
const cap = 4 * 1024 * 1024
const privateMarker = 'config-editor-private-fixture'
const initial = `mode: rule\r\nport: 7890\r\nsecret: ${privateMarker}\r\n`
const firstFile = { path: '/fixture/first.yaml', content: initial, revision: 'revision-1', max_size: cap }
const secondFile = { path: '/fixture/second.yaml', content: 'mode: direct\n# second-controller\n', revision: 'second-1', max_size: cap }
const reads = []
const saves = []
const runtimeWrites = []
const pageErrors = []
let getMode = 'normal'
let saveMode = 'hold'
let heldRead
let heldSave
let browser

function json(res, status, body) {
    res.writeHead(status, { 'content-type': 'application/json', 'cache-control': 'no-store' })
    res.end(JSON.stringify(body))
}
async function fixtureReady(check) {
    const deadline = Date.now() + 3000
    while (!check() && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 10))
    assert.ok(check(), 'controller fixture received the request')
}
function handle(req, res) {
    res.setHeader('Access-Control-Allow-Origin', 'http://127.0.0.1:' + server.address().port)
    res.setHeader('Access-Control-Allow-Headers', 'Authorization, Content-Type')
    res.setHeader('Access-Control-Allow-Methods', 'GET, PUT, PATCH, OPTIONS')
    if (req.method === 'OPTIONS') { res.writeHead(204); res.end(); return }
    const pathname = new URL(req.url, 'http://127.0.0.1').pathname
    const secondary = req.socket.localPort === secondServer.address()?.port
    const file = secondary ? secondFile : firstFile
    if (pathname === '/configs/file' && req.method === 'GET') {
        reads.push(req.socket.localPort)
        if (!secondary && getMode === 'hold') { heldRead = res; return }
        if (getMode === 'unsupported') { json(res, 404, { message: 'Not found' }); return }
        json(res, 200, file); return
    }
    if (pathname === '/configs/file' && req.method === 'PUT') {
        let body = ''
        req.on('data', chunk => { body += chunk })
        req.on('end', () => {
            const payload = JSON.parse(body)
            saves.push({ port: req.socket.localPort, payload })
            if (saveMode === 'hold') { heldSave = { res, payload, file }; return }
            if (saveMode === 'conflict') { json(res, 409, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (saveMode === 'invalid') { json(res, 400, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (saveMode === 'format') { json(res, 415, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (saveMode === 'path') { json(res, 422, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (saveMode === 'busy') { json(res, 503, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            file.content = payload.content
            file.revision += '-saved'
            json(res, 200, file)
        })
        return
    }
    if (pathname === '/configs' && req.method !== 'GET') {
        runtimeWrites.push(req.method)
        res.writeHead(204); res.end(); return
    }
    const api = {
        '/version': { version: 'isolated-editor-fixture', premium: false },
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
}
const server = http.createServer(handle)
const secondServer = http.createServer(handle)
const sockets = [new WebSocketServer({ server }), new WebSocketServer({ server: secondServer })]
for (const wss of sockets) wss.on('connection', socket => socket.send(JSON.stringify({ connections: [], uploadTotal: 0, downloadTotal: 0 })))

async function main() {
    await Promise.all([new Promise(resolve => server.listen(0, '127.0.0.1', resolve)), new Promise(resolve => secondServer.listen(0, '127.0.0.1', resolve))])
    const port = server.address().port
    const secondPort = secondServer.address().port
    browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined, args: process.env.PLAYWRIGHT_CHROMIUM_ARGS ? JSON.parse(process.env.PLAYWRIGHT_CHROMIUM_ARGS) : [] })
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'en-US', serviceWorkers: 'block' })
    await context.route('**/*', route => {
        const url = new URL(route.request().url())
        if (url.hostname === '127.0.0.1' && [String(port), String(secondPort)].includes(url.port)) return route.continue()
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
    await page.goto('http://127.0.0.1:' + port + '/#/settings', { waitUntil: 'networkidle' })
    const editor = page.locator('.config-editor-content')
    const text = editor.locator('textarea')
    const save = () => editor.getByRole('button', { name: 'Save file', exact: true })
    const close = () => editor.getByRole('button', { name: 'Close', exact: true })
    const open = async () => {
        await page.getByRole('button', { name: 'Edit configuration file', exact: true }).click()
        await editor.waitFor()
    }
    const waitLoaded = async value => { await page.waitForFunction(value => document.querySelector('.config-editor-text')?.value === value && !document.querySelector('.config-editor-text')?.disabled, value) }
    await open()
    await waitLoaded(initial.replaceAll('\r\n', '\n'))
    assert.equal(await save().isDisabled(), true, 'opening a CRLF file does not create an unsaved edit')

    const edited = initial.replaceAll('\r\n', '\n').replace('7890', '7892')
    await text.fill(edited)
    const received = page.waitForRequest(request => new URL(request.url()).pathname === '/configs/file' && request.method() === 'PUT')
    await save().click()
    await received
    await fixtureReady(() => heldSave)
    await editor.getByRole('button', { name: 'Saving…', exact: true }).waitFor()
    await text.press('Control+s')
    assert.equal(saves.length, 1, 'a second Ctrl-S while saving cannot duplicate the write')
    assert.deepEqual(saves[0].payload, { content: edited.replaceAll('\n', '\r\n'), revision: 'revision-1' }, 'file save sends only content and its original revision, preserving CRLF')
    firstFile.content = heldSave.payload.content
    firstFile.revision = 'revision-2'
    json(heldSave.res, 200, firstFile); heldSave = null
    await editor.getByText('Configuration file saved. The running configuration is unchanged. Use Reload configuration to apply your changes.', { exact: true }).waitFor()
    assert.deepEqual(runtimeWrites, [], 'saving never patches or reloads runtime configuration')

    saveMode = 'invalid'
    const badDraft = 'mode: [\n# preserve-invalid-draft\n'
    await text.fill(badDraft)
    await text.press('Control+s')
    await editor.getByRole('alert').getByText(/YAML syntax or basic structure validation failed/).waitFor()
    assert.equal(await text.inputValue(), badDraft)
    assert.equal(await save().isEnabled(), true, 'a caught rejected Ctrl-S can be corrected and retried')
    assert.equal(await page.getByText('PRIVATE-SERVER-ERROR-MARKER', { exact: false }).count(), 0, 'server exception text is never echoed into notifications')

    for (const [mode, message] of [
        ['format', /plaintext UTF-8 configuration files/],
        ['path', /regular file with no symbolic links/],
        ['busy', /Another operation is using the configuration file/],
    ]) {
        saveMode = mode
        await save().click()
        await editor.getByRole('alert').getByText(message).waitFor()
        assert.equal(await text.inputValue(), badDraft, mode + ' errors preserve the draft')
    }

    saveMode = 'conflict'
    const draft = '# unique-unsaved-draft\nmode: global\n'
    await text.fill(draft)
    await save().click()
    await editor.getByRole('alert').getByText(/The file was changed by another operation/).waitFor()
    assert.equal(await text.inputValue(), draft)
    assert.equal(await save().isDisabled(), true)
    await close().click()
    await editor.getByRole('button', { name: 'Keep editing', exact: true }).click()
    assert.equal(await text.inputValue(), draft, 'canceling discard keeps the conflicting draft')
    await editor.getByRole('button', { name: 'Read from file again', exact: true }).click()
    await editor.getByRole('button', { name: 'Discard and read again', exact: true }).click()
    await waitLoaded(edited)
    await text.fill(draft)
    await text.press('Escape')
    await editor.getByRole('button', { name: 'Discard and close', exact: true }).click()
    await editor.waitFor({ state: 'detached' })
    await open()
    await waitLoaded(edited)
    assert.equal(await text.inputValue(), edited, 'reopening loads the file instead of retaining the discarded draft')

    await close().click()
    getMode = 'unsupported'
    await open()
    await editor.getByRole('alert').getByText(/does not support the configuration file editor/).waitFor()
    assert.equal(await save().isDisabled(), true)
    await close().click()

    getMode = 'normal'
    firstFile.content = '# large-file\n' + '# line\n'.repeat(20000)
    await open()
    await waitLoaded(firstFile.content)
    await text.press('Control+End')
    await page.waitForFunction(() => document.querySelector('.config-editor-gutter pre')?.textContent.trim().endsWith('20002')).catch(async reason => {
        const metrics = await text.evaluate(element => ({ length: element.value.length, caret: element.selectionStart, scrollTop: element.scrollTop, height: element.clientHeight, scrollHeight: element.scrollHeight, lineHeight: getComputedStyle(element).lineHeight, gutter: document.querySelector('.config-editor-gutter pre')?.textContent, transform: document.querySelector('.config-editor-gutter pre')?.style.transform }))
        throw new Error(reason.message + '\nEditor viewport metrics: ' + JSON.stringify(metrics))
    })
    assert.ok((await editor.locator('.config-editor-gutter pre').textContent()).split('\n').length < 100, 'large files render only visible line numbers')
    await close().click()

    getMode = 'hold'
    await open()
    await fixtureReady(() => heldRead)
    const canceled = page.waitForEvent('requestfailed', { predicate: request => new URL(request.url()).pathname === '/configs/file' && new URL(request.url()).port === String(port) })
    await page.evaluate(secondPort => { location.hash = '/settings?port=' + secondPort + '&protocol=http:' }, secondPort)
    await canceled
    await editor.waitFor({ state: 'detached' })
    await open()
    await waitLoaded(secondFile.content)
    json(heldRead, 200, { ...firstFile, content: '# stale-first-controller-secret\n' }); heldRead = null
    assert.equal(await text.inputValue(), secondFile.content, 'an obsolete controller response cannot replace the new draft')
    saveMode = 'normal'
    const secondDraft = secondFile.content + '# second edit\n'
    await text.fill(secondDraft)
    await save().click()
    await editor.getByText('Configuration file saved. The running configuration is unchanged. Use Reload configuration to apply your changes.', { exact: true }).waitFor()
    assert.equal(saves.at(-1).port, secondPort)
    assert.equal(saves.at(-1).payload.revision, 'second-1')
    const persistent = await page.evaluate(() => [Object.values(localStorage), Object.values(sessionStorage)].flat().join('\n'))
    assert.equal(persistent.includes(privateMarker) || persistent.includes('unique-unsaved-draft') || persistent.includes('revision-1'), false, 'configuration text, drafts and revisions never enter browser storage')
    assert.deepEqual(runtimeWrites, [])
    assert.deepEqual(pageErrors, [], 'Ctrl-S failures and request cancellations do not produce unhandled rejections')
    console.log(JSON.stringify({ passed: true, reads: reads.length, saves: saves.length, fileOnly: true, conflictDraftPreserved: true, hostSwitchIsolated: true, boundedGutter: true, memoryOnly: true, pageErrors }, null, 2))
}

main().catch(error => { console.error(error.stack); process.exitCode = 1 }).finally(async () => {
    if (browser) await browser.close()
    heldRead?.destroy()
    heldSave?.res.destroy()
    for (const wss of sockets) { for (const socket of wss.clients) socket.terminate(); wss.close() }
    server.close(); secondServer.close()
})
