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
const firstFile = { path: '/fixture/first.yaml', content: initial, revision: 'revision-1', max_size: cap, writable: true }
const secondFile = { path: '/fixture/second.yaml', content: 'mode: direct\n# second-controller\n', revision: 'second-1', max_size: cap }
const reads = []
const saves = []
const runtimeWrites = []
const generalReads = []
const operations = []
const pageErrors = []
let getMode = 'normal'
let saveMode = 'hold'
let reloadMode = 'hold'
let generalMode = 'normal'
let heldRead
let heldSave
let heldReload
let committedWrites = 0
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
function completeSave({ res, payload, file, port }) {
    if (payload.revision !== file.revision) { json(res, 409, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
    // The core checks the revision even when a retry has no new text to write.
    if (file.content !== payload.content) {
        file.content = payload.content
        file.revision += '-saved'
        committedWrites++
    }
    operations.push({ stage: 'save-success', port })
    json(res, 200, file)
}
function completeReload({ res, port }) {
    operations.push({ stage: 'reload-success', port })
    res.writeHead(204); res.end()
}
function handle(req, res) {
    res.setHeader('Access-Control-Allow-Origin', 'http://127.0.0.1:' + server.address().port)
    res.setHeader('Access-Control-Allow-Headers', 'Authorization, Content-Type')
    res.setHeader('Access-Control-Allow-Methods', 'GET, PUT, PATCH, OPTIONS')
    if (req.method === 'OPTIONS') { res.writeHead(204); res.end(); return }
    const url = new URL(req.url, 'http://127.0.0.1')
    const pathname = url.pathname
    const port = req.socket.localPort
    const secondary = req.socket.localPort === secondServer.address()?.port
    const file = secondary ? secondFile : firstFile
    if (pathname === '/configs/file' && req.method === 'GET') {
        reads.push(req.socket.localPort)
        if (!secondary && getMode === 'hold') { heldRead = res; return }
        if (getMode === 'unsupported') { json(res, 404, { message: 'Not found' }); return }
        if (getMode === 'fileAccess') { json(res, 403, { code: 'config_file_access_denied', message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
        json(res, 200, file); return
    }
    if (pathname === '/configs/file' && req.method === 'PUT') {
        let body = ''
        req.on('data', chunk => { body += chunk })
        req.on('end', () => {
            const payload = JSON.parse(body)
            saves.push({ port, payload })
            operations.push({ stage: 'save-request', port })
            if (saveMode === 'hold') { heldSave = { res, payload, file, port }; return }
            if (saveMode === 'conflict') { json(res, 409, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (saveMode === 'invalid') { json(res, 400, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (saveMode === 'format') { json(res, 415, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (saveMode === 'path') { json(res, 422, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (saveMode === 'busy') { json(res, 503, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (saveMode === 'network') { json(res, 500, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (saveMode === 'unauthorized') { json(res, 403, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (saveMode === 'readOnly' || saveMode === 'fileAccess') {
                file.writable = false
                json(res, 403, { code: saveMode === 'readOnly' ? 'config_file_read_only' : 'config_file_access_denied', message: 'PRIVATE-SERVER-ERROR-MARKER' }); return
            }
            if (saveMode === 'malformed') { json(res, 200, { ...file, revision: '', content: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            completeSave({ res, payload, file, port })
        })
        return
    }
    if (pathname === '/configs' && req.method !== 'GET') {
        let body = ''
        req.on('data', chunk => { body += chunk })
        req.on('end', () => {
            runtimeWrites.push({ port, method: req.method, url: req.url, payload: JSON.parse(body || 'null') })
            operations.push({ stage: 'reload-request', port })
            if (reloadMode === 'hold') { heldReload = { res, port }; return }
            if (reloadMode === 'invalid') { json(res, 400, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            if (reloadMode === 'network') { json(res, 503, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
            completeReload({ res, port })
        })
        return
    }
    if (pathname === '/configs' && req.method === 'GET') {
        generalReads.push({ port, mode: generalMode })
        operations.push({ stage: 'general-read', port })
        if (generalMode === 'fail') { json(res, 503, { message: 'PRIVATE-SERVER-ERROR-MARKER' }); return }
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
    const save = () => editor.getByRole('button', { name: 'Save and reload', exact: true })
    const retry = () => editor.getByRole('button', { name: 'Retry reload', exact: true })
    const close = () => editor.getByRole('button', { name: 'Close', exact: true })
    const saved = () => editor.getByText('Configuration file saved and reloaded.', { exact: true })
    const invalidReload = 'The file was saved, but full configuration validation failed. Check the configuration, then save and reload again.'
    const uncertainReload = 'The file was saved, but a successful reload could not be confirmed. Check the controller connection and configuration, then retry the reload.'
    const open = async () => {
        await page.getByRole('button', { name: 'Edit configuration file', exact: true }).click()
        await editor.waitFor()
    }
    const waitLoaded = async value => { await page.waitForFunction(value => document.querySelector('.config-editor-text')?.value === value && !document.querySelector('.config-editor-text')?.disabled, value) }
    const assertFileStart = async () => {
        await page.waitForFunction(() => {
            const area = document.querySelector('.config-editor-text')
            return area && document.activeElement === area && area.selectionStart === 0 && area.selectionEnd === 0 && area.scrollTop === 0 && area.scrollLeft === 0
        })
        assert.match(await editor.locator('.config-editor-status').textContent(), /Ln 1 · Col 1/)
        assert.equal((await editor.locator('.config-editor-gutter pre').textContent()).trim().split('\n')[0], '1')
    }
    const assertDrawnIcon = async (container, label) => {
        const paths = await container.locator('svg path').evaluateAll(paths => paths.map(path => path.getAttribute('d')?.trim()))
        assert.ok(paths.length > 0 && paths.every(Boolean), label + ' renders nonempty SVG paths')
    }
    const assertBusy = async label => {
        const button = editor.getByRole('button', { name: label, exact: true })
        await button.waitFor()
        assert.equal(await button.isDisabled(), true)
        assert.equal(await close().isDisabled(), true, 'close stays guarded during ' + label)
        assert.equal(await text.getAttribute('readonly'), '', 'text stays read-only during ' + label)
        await text.press('Control+s')
        await button.evaluate(element => element.click())
        await text.press('Escape')
        await page.waitForTimeout(60)
        assert.equal(await editor.isVisible(), true, 'Escape cannot dismiss an in-flight ' + label)
        assert.equal(await editor.locator('.config-editor-confirm').count(), 0)
    }
    await assertDrawnIcon(page.getByRole('button', { name: 'Edit configuration file', exact: true }), 'edit action')
    await open()
    await waitLoaded(initial.replaceAll('\r\n', '\n'))
    await assertFileStart()
    await assertDrawnIcon(editor.locator('.config-editor-file'), 'configuration file')
    await assertDrawnIcon(save(), 'save and reload action')
    assert.equal(await save().isDisabled(), true, 'opening a CRLF file does not create an unsaved edit')

    const edited = initial.replaceAll('\r\n', '\n').replace('7890', '7892')
    await text.fill(edited)
    const firstOperation = operations.length
    const received = page.waitForRequest(request => new URL(request.url()).pathname === '/configs/file' && request.method() === 'PUT')
    await save().click()
    await received
    await fixtureReady(() => heldSave)
    await assertBusy('Saving…')
    assert.equal(saves.length, 1, 'clicks and Ctrl-S while saving cannot duplicate the write')
    assert.equal(runtimeWrites.length, 0, 'reload cannot start before the file save succeeds')
    assert.deepEqual(saves[0].payload, { content: edited.replaceAll('\n', '\r\n'), revision: 'revision-1' }, 'file save sends only content and its original revision, preserving CRLF')
    completeSave(heldSave); heldSave = null
    await fixtureReady(() => heldReload)
    await assertBusy('Reloading…')
    assert.equal(saves.length, 1, 'Ctrl-S during reload cannot begin another save')
    assert.equal(runtimeWrites.length, 1, 'Ctrl-S during reload cannot duplicate reload')
    assert.deepEqual(runtimeWrites[0], { port, method: 'PUT', url: '/configs?force=true', payload: {} }, 'reload uses the same controller and its current file, with no path or configuration payload')
    assert.deepEqual(operations.slice(firstOperation).map(operation => operation.stage), ['save-request', 'save-success', 'reload-request'], 'runtime settings are not refreshed while reload is pending')
    completeReload(heldReload); heldReload = null
    await saved().waitFor()
    await fixtureReady(() => operations.slice(firstOperation).some(operation => operation.stage === 'general-read'))
    assert.deepEqual(operations.slice(firstOperation).map(operation => operation.stage), ['save-request', 'save-success', 'reload-request', 'reload-success', 'general-read'], 'the settings snapshot is refreshed only after reload succeeds')
    assert.equal(await save().isDisabled(), true)
    reloadMode = 'normal'

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
        ['network', /configuration file request could not be completed/],
        ['unauthorized', /controller denied this request/],
        ['malformed', /did not return an editable configuration file/],
    ]) {
        saveMode = mode
        await save().click()
        await editor.getByRole('alert').getByText(message).waitFor()
        assert.equal(await text.inputValue(), badDraft, mode + ' errors preserve the draft')
        if (mode === 'unauthorized') assert.equal(await text.evaluate(element => element.readOnly), false, 'controller authentication errors do not claim the file is read-only')
    }
    assert.equal(runtimeWrites.length, 1, 'rejected saves and malformed save responses never reload the configuration')

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

    saveMode = 'normal'
    reloadMode = 'invalid'
    const rejectedDraft = edited + '# full-validation-fixture\n'
    await text.fill(rejectedDraft)
    await save().click()
    await editor.getByRole('alert').getByText(invalidReload, { exact: true }).waitFor()
    const savedRevision = firstFile.revision
    const writesBeforeRetry = committedWrites
    assert.equal(await text.inputValue(), rejectedDraft, 'reload failure keeps the newly saved text')
    assert.equal((await editor.locator('.config-editor-state').textContent()).includes('Unsaved changes'), false, 'a saved file is clean even when reload fails')
    assert.equal(await retry().isEnabled(), true, 'a clean saved file can retry reload without a dummy edit')

    reloadMode = 'network'
    await retry().click()
    await editor.getByRole('alert').getByText(uncertainReload, { exact: true }).waitFor()
    assert.deepEqual(saves.at(-1).payload, { content: rejectedDraft.replaceAll('\n', '\r\n'), revision: savedRevision }, 'retry checks the latest saved revision before reloading')
    assert.equal(committedWrites, writesBeforeRetry, 'a clean retry uses the core no-op save path')
    assert.equal(await retry().isEnabled(), true)
    reloadMode = 'normal'
    await retry().click()
    await saved().waitFor()
    assert.equal(firstFile.revision, savedRevision, 'a successful clean retry preserves the existing file revision')
    assert.equal(committedWrites, writesBeforeRetry)
    assert.equal(await save().isDisabled(), true)

    reloadMode = 'invalid'
    const correctionDraft = rejectedDraft + '# needs-correction\n'
    await text.fill(correctionDraft)
    await save().click()
    await editor.getByRole('alert').getByText(invalidReload, { exact: true }).waitFor()
    const correctionRevision = firstFile.revision
    const correctedDraft = correctionDraft.replace('needs-correction', 'corrected')
    await text.fill(correctedDraft)
    assert.equal(await save().isEnabled(), true, 'editing after a reload error restores the Save and reload action')
    reloadMode = 'normal'
    await save().click()
    await saved().waitFor()
    assert.deepEqual(saves.at(-1).payload, { content: correctedDraft.replaceAll('\n', '\r\n'), revision: correctionRevision }, 'the correction saves against the revision returned before reload failed')

    reloadMode = 'network'
    const retryConflictDraft = correctedDraft + '# retry-conflict\n'
    await text.fill(retryConflictDraft)
    await save().click()
    await editor.getByRole('alert').getByText(uncertainReload, { exact: true }).waitFor()
    const revisionBeforeExternalWrite = firstFile.revision
    firstFile.content = 'mode: direct\n# newer-external-file\n'
    firstFile.revision = 'external-revision'
    const reloadsBeforeConflict = runtimeWrites.length
    await retry().click()
    await editor.getByRole('alert').getByText(/The file was changed by another operation/).waitFor()
    assert.equal(saves.at(-1).payload.revision, revisionBeforeExternalWrite)
    assert.equal(runtimeWrites.length, reloadsBeforeConflict, 'a newer external file blocks retry before it can reload unrelated changes')
    assert.equal(firstFile.content, 'mode: direct\n# newer-external-file\n', 'retry does not overwrite the newer external configuration')
    assert.equal(await text.inputValue(), retryConflictDraft, 'retry conflict preserves the editor text')
    assert.equal(await retry().isDisabled(), true)
    await editor.getByRole('button', { name: 'Read from file again', exact: true }).click()
    await waitLoaded(firstFile.content)

    reloadMode = 'normal'
    generalMode = 'fail'
    const refreshDraft = firstFile.content + '# successful-reload-failed-refresh\n'
    await text.fill(refreshDraft)
    const failedReadsBeforeReload = generalReads.filter(read => read.mode === 'fail').length
    const failedRefresh = page.waitForResponse(response => new URL(response.url()).pathname === '/configs' && response.request().method() === 'GET' && response.status() === 503)
    await save().click()
    await fixtureReady(() => generalReads.filter(read => read.mode === 'fail').length > failedReadsBeforeReload)
    await (await failedRefresh).finished()
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))))
    await saved().waitFor()
    assert.equal(await editor.getByRole('alert').count(), 0, 'a failed settings refresh cannot turn a completed reload into an error')
    assert.equal(await save().isDisabled(), true)
    generalMode = 'normal'

    await close().click()
    firstFile.writable = false
    await open()
    await waitLoaded(firstFile.content)
    await assertFileStart()
    assert.equal(await text.evaluate(element => element.readOnly && !element.disabled), true, 'a readable but unwritable file supports selection and copying')
    assert.equal((await editor.locator('.config-editor-state').textContent()).trim(), 'Read only')
    await editor.getByText(/The kernel cannot write this configuration file/).waitFor()
    const readonlyContents = await text.inputValue()
    const savesBeforeReadonly = saves.length
    const reloadsBeforeReadonly = runtimeWrites.length
    for (const key of ['a', 'Backspace', 'Enter', 'Tab', 'Control+s', 'Meta+s']) await text.press(key)
    await save().evaluate(element => element.click())
    assert.equal(await text.inputValue(), readonlyContents, 'typing, deletion and indentation cannot alter a read-only document')
    assert.equal(await save().isDisabled(), true)
    assert.equal(saves.length, savesBeforeReadonly, 'read-only shortcuts and clicks never send a save')
    assert.equal(runtimeWrites.length, reloadsBeforeReadonly)
    await text.press('Control+a')
    assert.equal(await text.evaluate(element => element.selectionEnd - element.selectionStart), readonlyContents.length, 'read-only text remains selectable')
    await close().click()
    await editor.waitFor({ state: 'detached' })

    for (const mode of ['readOnly', 'fileAccess']) {
        firstFile.writable = true
        saveMode = mode
        await open()
        await waitLoaded(firstFile.content)
        const permissionDraft = firstFile.content + '# permission-loss-draft\n'
        await text.fill(permissionDraft)
        const beforeSave = saves.length
        const beforeReload = runtimeWrites.length
        await save().click()
        await editor.getByRole('alert').getByText(mode === 'readOnly' ? /can no longer write this configuration file/ : /cannot read this configuration file/).waitFor()
        assert.equal(await text.inputValue(), permissionDraft, 'permission loss preserves the entire draft')
        assert.equal(await text.evaluate(element => element.readOnly), true)
        assert.equal(await save().isDisabled(), true)
        await text.press('Control+s')
        assert.equal(saves.length, beforeSave + 1, 'permission loss blocks another save until capabilities are reread')
        assert.equal(runtimeWrites.length, beforeReload, 'a permission-denied save never reloads')
        await editor.getByRole('button', { name: 'Read from file again', exact: true }).click()
        await editor.getByRole('button', { name: 'Discard and read again', exact: true }).click()
        await waitLoaded(firstFile.content)
        assert.equal(await text.evaluate(element => element.readOnly), true)
        firstFile.writable = true
        await editor.getByRole('button', { name: 'Read from file again', exact: true }).click()
        await page.waitForFunction(() => document.querySelector('.config-editor-text')?.readOnly === false)
        await assertFileStart()
        await close().click()
    }
    saveMode = 'normal'
    getMode = 'fileAccess'
    await open()
    await editor.getByRole('alert').getByText(/The kernel cannot read this configuration file/).waitFor()
    assert.equal(await text.isDisabled(), true, 'unreadable files have an explicit permission error and no editable content')
    assert.equal(await save().isDisabled(), true)
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
    await assertFileStart()
    await text.press('Control+End')
    await page.waitForFunction(() => document.querySelector('.config-editor-gutter pre')?.textContent.trim().endsWith('20002')).catch(async reason => {
        const metrics = await text.evaluate(element => ({ length: element.value.length, caret: element.selectionStart, scrollTop: element.scrollTop, height: element.clientHeight, scrollHeight: element.scrollHeight, lineHeight: getComputedStyle(element).lineHeight, gutter: document.querySelector('.config-editor-gutter pre')?.textContent, transform: document.querySelector('.config-editor-gutter pre')?.style.transform }))
        throw new Error(reason.message + '\nEditor viewport metrics: ' + JSON.stringify(metrics))
    })
    assert.ok((await editor.locator('.config-editor-gutter pre').textContent()).split('\n').length < 100, 'large files render only visible line numbers')
    await editor.getByRole('button', { name: 'Read from file again', exact: true }).click()
    await waitLoaded(firstFile.content)
    await assertFileStart()
    await text.press('Control+Home')
    await page.keyboard.insertText('# preserve-reading-position\n')
    assert.ok((await text.inputValue()).startsWith('# preserve-reading-position\n'), 'editing a large file updates its existing draft')
    const readingPosition = await text.evaluate(element => {
        element.setSelectionRange(2200, 2200)
        element.scrollTop = 6000
        element.scrollLeft = 0
        element.dispatchEvent(new Event('scroll', { bubbles: true }))
        return { caret: element.selectionStart, top: element.scrollTop, left: element.scrollLeft }
    })
    await save().click()
    await saved().waitFor()
    await page.waitForTimeout(180)
    assert.deepEqual(await text.evaluate(element => ({ caret: element.selectionStart, top: element.scrollTop, left: element.scrollLeft })), readingPosition, 'a successful save does not reset the caret or reading position')
    assert.equal(await text.evaluate(element => document.activeElement === element), false, 'a save response does not steal focus back from the action button')
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
    assert.equal(await text.evaluate(element => element.readOnly), false, 'older cores without writable metadata retain their editing behavior')
    json(heldRead, 200, { ...firstFile, content: '# stale-first-controller-secret\n' }); heldRead = null
    assert.equal(await text.inputValue(), secondFile.content, 'an obsolete controller response cannot replace the new draft')

    // Switching during the save must abort the old session before it can send
    // a reload, even if the old controller completes its write afterwards.
    getMode = 'normal'
    saveMode = 'hold'
    const abandonedDraft = secondFile.content + '# abandoned-controller-save\n'
    await text.fill(abandonedDraft)
    await save().click()
    await fixtureReady(() => heldSave)
    const reloadsBeforeSaveSwitch = runtimeWrites.length
    const canceledSave = page.waitForEvent('requestfailed', { predicate: request => new URL(request.url()).pathname === '/configs/file' && request.method() === 'PUT' && new URL(request.url()).port === String(secondPort) })
    firstFile.content = 'mode: rule\n# first-controller-return\n'
    firstFile.revision = 'first-return-revision'
    await page.evaluate(port => { location.hash = '/settings?port=' + port + '&protocol=http:' }, port)
    await canceledSave
    await editor.waitFor({ state: 'detached' })
    await open()
    await waitLoaded(firstFile.content)
    completeSave(heldSave); heldSave = null
    await page.waitForTimeout(80)
    assert.equal(runtimeWrites.length, reloadsBeforeSaveSwitch, 'an abandoned save cannot start a reload on either controller')
    assert.equal(await text.inputValue(), firstFile.content, 'the old save cannot replace the new controller text')
    assert.equal(await saved().count(), 0, 'the abandoned save cannot report success in the new editor')

    // Once reload has begun, switching also aborts that request and prevents
    // its eventual response from refreshing or marking the new editor saved.
    saveMode = 'normal'
    reloadMode = 'hold'
    await text.fill(firstFile.content + '# held-reload-switch\n')
    await save().click()
    await fixtureReady(() => heldReload)
    const oldGeneralReads = generalReads.filter(read => read.port === port).length
    const canceledReload = page.waitForEvent('requestfailed', { predicate: request => new URL(request.url()).pathname === '/configs' && request.method() === 'PUT' && new URL(request.url()).port === String(port) })
    await page.evaluate(secondPort => { location.hash = '/settings?port=' + secondPort + '&protocol=http:' }, secondPort)
    await canceledReload
    await editor.waitFor({ state: 'detached' })
    await open()
    await waitLoaded(secondFile.content)
    completeReload(heldReload); heldReload = null
    await page.waitForTimeout(80)
    assert.equal(generalReads.filter(read => read.port === port).length, oldGeneralReads, 'an abandoned reload does not refresh the old controller after switching')
    assert.equal(await text.inputValue(), secondFile.content, 'the old reload cannot alter the new controller draft')
    assert.equal(await saved().count(), 0, 'an obsolete reload response cannot display success in the new editor')

    saveMode = 'normal'
    reloadMode = 'normal'
    const secondDraft = secondFile.content + '# second edit\n'
    const secondRevision = secondFile.revision
    await text.fill(secondDraft)
    await save().click()
    await saved().waitFor()
    assert.equal(saves.at(-1).port, secondPort)
    assert.equal(saves.at(-1).payload.revision, secondRevision)
    assert.equal(runtimeWrites.at(-1).port, secondPort, 'the active controller owns both stages of save and reload')
    for (const { method, url, payload } of runtimeWrites) assert.deepEqual({ method, url, payload }, { method: 'PUT', url: '/configs?force=true', payload: {} })
    const persistent = await page.evaluate(() => [Object.values(localStorage), Object.values(sessionStorage)].flat().join('\n'))
    assert.equal(persistent.includes(privateMarker) || persistent.includes('unique-unsaved-draft') || persistent.includes('revision-1'), false, 'configuration text, drafts and revisions never enter browser storage')
    assert.equal(await page.getByText('PRIVATE-SERVER-ERROR-MARKER', { exact: false }).count(), 0, 'save, reload and refresh exceptions never expose server text')
    assert.deepEqual(pageErrors, [], 'Ctrl-S failures and request cancellations do not produce unhandled rejections')
    await close().click()

    const mobile = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, locale: 'en-US', serviceWorkers: 'block' })
    await mobile.route('**/*', route => {
        const url = new URL(route.request().url())
        return url.hostname === '127.0.0.1' && [String(port), String(secondPort)].includes(url.port) ? route.continue() : route.abort()
    })
    await mobile.addInitScript(({ port }) => {
        localStorage.setItem('language', JSON.stringify('en_US'))
        localStorage.setItem('externalControllers', JSON.stringify([{ hostname: '127.0.0.1', port, secret: '' }]))
        localStorage.setItem('externalControllerIndex', '0')
    }, { port })
    const phone = await mobile.newPage()
    phone.on('pageerror', error => pageErrors.push(String(error)))
    await phone.goto('http://127.0.0.1:' + port + '/#/settings', { waitUntil: 'networkidle' })
    const mobileSizes = [{ width: 320, height: 640 }, { width: 390, height: 844 }, { width: 844, height: 390 }]
    for (const size of mobileSizes) {
        await phone.setViewportSize(size)
        await phone.getByRole('button', { name: 'Edit configuration file', exact: true }).click()
        await phone.waitForFunction(() => {
            const area = document.querySelector('.config-editor-text')
            const rect = document.querySelector('.config-editor-modal')?.getBoundingClientRect()
            return area && !area.disabled && area.selectionStart === 0 && area.scrollTop === 0 && rect && Math.abs(rect.top) < 1 && Math.abs(rect.left) < 1 && Math.abs(rect.width - innerWidth) < 1 && Math.abs(rect.height - innerHeight) < 1
        })
        const bounds = await phone.locator('.config-editor-modal').evaluate(element => ({ radius: getComputedStyle(element).borderTopLeftRadius, overflow: element.scrollWidth - element.clientWidth }))
        assert.equal(bounds.radius, '0px', 'the mobile editor fills the viewport without sheet corners')
        assert.ok(bounds.overflow < 2, 'the mobile editor has no horizontal overflow')
        const phoneClose = phone.locator('.config-editor-content').getByRole('button', { name: 'Close', exact: true })
        await phoneClose.scrollIntoViewIfNeeded()
        const closeBounds = await phoneClose.boundingBox()
        assert.ok(closeBounds.y >= 0 && closeBounds.y + closeBounds.height <= size.height + 1, 'footer actions remain reachable, including in landscape')
        await phoneClose.click()
        await phone.locator('.config-editor-content').waitFor({ state: 'detached' })
    }
    await mobile.close()
    assert.deepEqual(pageErrors, [])
    console.log(JSON.stringify({ passed: true, reads: reads.length, saves: saves.length, reloads: runtimeWrites.length, sequentialSaveAndReload: true, duplicateAndCloseGuard: true, revisionCheckedRetry: true, saveErrorsNeverReload: true, refreshFailureIsolated: true, editorIconsVisible: true, conflictDraftPreserved: true, hostSwitchIsolated: true, boundedGutter: true, readOnlyBeforeEditing: true, permissionLossPreservesDraft: true, initialFilePosition: true, savePreservesPosition: true, mobileFullscreen: mobileSizes, memoryOnly: true, pageErrors }, null, 2))
}

main().catch(error => { console.error(error.stack); process.exitCode = 1 }).finally(async () => {
    if (browser) await browser.close()
    heldRead?.destroy()
    heldSave?.res.destroy()
    heldReload?.res.destroy()
    for (const wss of sockets) { for (const socket of wss.clients) socket.terminate(); wss.close() }
    server.close(); secondServer.close()
})
