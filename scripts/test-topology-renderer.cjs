const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const { test } = require('node:test')
const ts = require('typescript')

const filename = path.resolve(__dirname, '../src/containers/Proxies/components/Topology/draw.ts')
const compiled = ts.transpileModule(fs.readFileSync(filename, 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
}).outputText

class Events {
    listeners = new Map()
    addEventListener(type, callback) {
        if (!this.listeners.has(type)) this.listeners.set(type, new Set())
        this.listeners.get(type).add(callback)
    }
    removeEventListener(type, callback) { this.listeners.get(type)?.delete(callback) }
    emit(type, event = {}) { for (const callback of this.listeners.get(type) ?? []) callback(event) }
    count() { return [...this.listeners.values()].reduce((total, listeners) => total + listeners.size, 0) }
}

function fixture({ dark = false, bitmaps = false } = {}) {
    let now = 1000
    let nextFrame = 0
    const frames = new Map()
    const media = new Map()
    const surfaces = []
    const pictures = []
    const texts = []
    // Offscreen canvases (the link layer and sprites) share the same counters.
    const document = Object.assign(new Events(), {
        hidden: false,
        fonts: new Events(),
        createElement() {
            const ctx = context2d(true)
            const canvas = { width: 0, height: 0, getContext: () => ctx }
            surfaces.push(canvas)
            return canvas
        },
    })
    const window = Object.assign(new Events(), {
        devicePixelRatio: 1,
        matchMedia(query) {
            if (!media.has(query)) media.set(query, Object.assign(new Events(), { matches: false }))
            return media.get(query)
        },
    })
    const intersections = []
    const resizes = []
    class IntersectionObserver {
        constructor(callback) { this.callback = callback; this.connected = true; intersections.push(this) }
        observe() {}
        disconnect() { this.connected = false }
        emit(isIntersecting) { if (this.connected) this.callback([{ isIntersecting }]) }
    }
    class ResizeObserver {
        constructor(callback) { this.callback = callback; this.connected = true; resizes.push(this) }
        observe() {}
        disconnect() { this.connected = false }
        emit() { if (this.connected) this.callback([]) }
    }
    const metrics = { frames: 0, measurements: 0, arcs: 0, stamps: 0, strokes: 0, layerPaints: 0, blurs: 0, texts: 0 }
    function context2d(offscreen) {
        const context = {
            font: '',
            shadowBlur: 0,
            measureText(text) { metrics.measurements++; return { width: Array.from(text).length * 7 } },
            clearRect() { if (offscreen) metrics.layerPaints++; else metrics.frames++ },
            arc() { metrics.arcs++ },
            createLinearGradient() { return { addColorStop() {} } },
            drawImage(image) {
                assert.ok(!image.closed && image.width > 0 && image.height > 0, 'a released image is never drawn')
                metrics.stamps++
            },
            stroke() { metrics.strokes++; if (context.shadowBlur > 0) metrics.blurs++ },
            fill() { if (context.shadowBlur > 0) metrics.blurs++ },
            fillText(text) { metrics.texts++; texts.push(text) },
        }
        for (const name of ['setTransform', 'save', 'restore', 'setLineDash', 'beginPath', 'moveTo', 'bezierCurveTo', 'roundRect', 'rect']) context[name] = () => {}
        return context
    }
    class OffscreenCanvas {
        constructor(width, height) { this.width = width; this.height = height; this.ctx = context2d(true) }
        getContext() { return this.ctx }
        transferToImageBitmap() {
            const image = {
                width: this.width, height: this.height, closed: false,
                close() { assert.equal(this.closed, false, 'a bitmap is released exactly once'); this.closed = true },
            }
            pictures.push(image)
            return image
        }
    }
    const ctx = context2d(false)
    const canvas = Object.assign(new Events(), {
        clientWidth: 900, clientHeight: 250, width: 0, height: 0, isConnected: true, style: {},
        getContext() { return ctx },
        getBoundingClientRect() { return { left: 0, top: 0 } },
    })
    const classes = new Set()
    const tooltip = {
        dataset: {}, style: {}, offsetWidth: 120,
        firstElementChild: { textContent: '' }, lastElementChild: { textContent: '' },
        classList: { add: name => classes.add(name), remove: name => classes.delete(name) },
    }
    const module = { exports: {} }
    const context = vm.createContext({
        exports: module.exports, module, window, document, IntersectionObserver, ResizeObserver,
        performance: { now: () => now },
        requestAnimationFrame(callback) { const id = ++nextFrame; frames.set(id, callback); return id },
        cancelAnimationFrame(id) { frames.delete(id) },
        getComputedStyle() { return { getPropertyValue: name => name === '--panel-solid' ? (dark ? '#0c1426' : '#ffffff') : '#0090c4' } },
        ...(bitmaps ? { OffscreenCanvas } : {}),
    })
    vm.runInContext(compiled, context, { filename })
    const picked = []
    const renderer = new module.exports.TopologyRenderer(canvas, tooltip, { onPick: target => picked.push(target) })
    return {
        renderer, canvas, tooltip, document, window, frames, media, intersections, resizes, metrics, picked, surfaces, pictures, texts,
        setDark(value) {
            dark = value
            media.get('(prefers-color-scheme: dark)').emit('change')
        },
        step(count = 1) {
            for (let i = 0; i < count; i++) {
                now += 1000 / 60
                const callbacks = [...frames.values()]
                frames.clear()
                for (const callback of callbacks) callback(now)
            }
        },
    }
}

function model(id, rate = 32768, longName = false) {
    return {
        rows: 3, headings: ['Source', 'Policy', 'Exit'],
        nodes: [
            { id: `s:${id}`, column: 0, kind: 'source', title: id, sub: '1 connection', tone: 'live' },
            { id: `g:${id}`, column: 1, kind: 'group', title: 'Policy', sub: 'Selected', tone: 'live' },
            { id: `x:${id}`, column: 2, kind: 'exit', title: longName ? '😀'.repeat(4096) : 'Node', sub: '25 ms', tone: 'good', stamp: `${id}:1`, target: { type: 'node', name: id } },
        ],
        links: [
            { from: `s:${id}`, to: `g:${id}`, rate, kind: 'proxy' },
            { from: `g:${id}`, to: `x:${id}`, rate, kind: 'proxy' },
        ],
    }
}

test('flow and exits animate; offscreen and background churn retain only the latest graph', () => {
    const f = fixture()
    f.renderer.setModel(model('initial'))
    f.step(120)
    assert.ok(f.renderer.particles.length > 0, 'traffic still produces moving particles')
    assert.ok(f.frames.size > 0, 'live flow still schedules animation')
    assert.ok(f.metrics.arcs > 0, 'particle and node drawing remains active')

    f.renderer.setModel(model('replacement'))
    const leaving = f.renderer.placed.get('s:initial')
    const alpha = leaving.alpha
    assert.equal(leaving.target, 0)
    f.step(2)
    assert.ok(leaving.alpha > 0 && leaving.alpha < alpha, 'visible outgoing node fades rather than disappearing')
    f.step(240)
    assert.equal(f.renderer.placed.size, 3)
    assert.equal(f.renderer.links.size, 2)
    assert.equal(f.renderer.stamps.size, 1, 'departed node test stamps are released')

    f.intersections[0].emit(false)
    assert.equal(f.frames.size, 0, 'offscreen work cancels its pending frame')
    for (let i = 0; i < 10000; i++) f.renderer.setModel(model(`offscreen-${i}`))
    assert.equal(f.renderer.placed.size, 3)
    assert.equal(f.renderer.links.size, 2)
    assert.equal(f.renderer.stamps.size, 1)
    assert.equal(f.renderer.particles.length, 0)
    assert.equal(f.frames.size, 0)
    assert.ok(f.renderer.placed.has('s:offscreen-9999'))

    f.intersections[0].emit(true)
    f.step(120)
    assert.ok(f.renderer.particles.length > 0, 'flow resumes when the canvas returns')
    f.document.hidden = true
    f.document.emit('visibilitychange')
    for (let i = 0; i < 10000; i++) f.renderer.setModel(model(`background-${i}`))
    assert.equal(f.renderer.placed.size, 3)
    assert.equal(f.renderer.links.size, 2)
    assert.equal(f.renderer.stamps.size, 1)
    assert.equal(f.frames.size, 0)
    f.document.hidden = false
    f.document.emit('visibilitychange')
    f.step(120)
    assert.ok(f.renderer.particles.length > 0)
    f.renderer.destroy()
})

test('steady flow stamps sprites each frame without repainting links, blurs or text', () => {
    const f = fixture({ dark: true })
    f.renderer.setModel(model('steady'))
    f.step(240)
    assert.ok(f.renderer.particles.length > 0, 'particles keep flowing')
    const before = { ...f.metrics }
    f.step(60)
    assert.equal(f.metrics.frames - before.frames, 60, 'every frame is still drawn')
    assert.ok(f.metrics.stamps - before.stamps > 60 * 7, 'boxes and particles are stamped every frame')
    assert.equal(f.metrics.layerPaints - before.layerPaints, 0, 'settled links are not repainted')
    assert.equal(f.metrics.strokes - before.strokes, 0, 'no link strokes while nothing about them changes')
    assert.equal(f.metrics.blurs - before.blurs, 0, 'no shadow blur per frame')
    assert.equal(f.metrics.texts - before.texts, 0, 'no text layout per frame')
    assert.ok(before.blurs > 0, 'dark glows were rendered once, into sprites')

    const next = model('steady', 65536)
    f.renderer.setModel(next)
    f.step(1)
    assert.equal(f.metrics.layerPaints - before.layerPaints, 1, 'new rates repaint the link layer once')
    f.renderer.destroy()
})

test('reduced motion settles immediately and resumes live flow when the preference changes', () => {
    const f = fixture()
    f.renderer.setModel(model('flow'))
    f.step(120)
    const preference = f.media.get('(prefers-reduced-motion: reduce)')
    preference.matches = true
    preference.emit('change')
    f.step(3)
    assert.equal(f.renderer.particles.length, 0)
    assert.equal(f.frames.size, 0, 'a settled reduced-motion graph has no perpetual RAF')
    preference.matches = false
    preference.emit('change')
    f.step(120)
    assert.ok(f.renderer.particles.length > 0, 'existing flow restarts without waiting for a new snapshot')
    f.renderer.destroy()
})

test('idle resize redraws geometry; long labels are fitted once and tooltips update while hovered', () => {
    const f = fixture()
    f.renderer.setModel(model('quiet', 0, true))
    f.step(240)
    assert.equal(f.frames.size, 0)
    assert.ok(f.metrics.measurements < 30, '4096-character label uses logarithmic fitting and a bounded per-node cache')
    const measurements = f.metrics.measurements
    f.canvas.clientWidth = 450
    f.resizes[0].emit()
    assert.ok(f.frames.size > 0, 'idle resize wakes the renderer')
    f.step(240)
    assert.equal(f.canvas.width, 450, 'the bitmap is resized, not merely stretched by CSS')
    assert.ok(f.metrics.measurements > measurements, 'labels refit to their new width')

    const exit = f.renderer.placed.get('x:quiet')
    f.canvas.emit('click', { clientX: exit.x + 15, clientY: exit.y + 15 })
    assert.equal(f.picked[0].name, 'quiet', 'touch/click works without a preceding pointermove or RAF')
    f.canvas.emit('pointermove', { clientX: exit.x + 15, clientY: exit.y + 15 })
    f.step(3)
    assert.equal(f.tooltip.lastElementChild.textContent, '25 ms')
    const updated = model('quiet', 0, true)
    updated.nodes[2].sub = '48 ms'
    updated.nodes[2].stamp = 'quiet:2'
    f.renderer.setModel(updated)
    f.step(2)
    assert.equal(f.tooltip.lastElementChild.textContent, '48 ms', 'same hovered node receives the latest reading')
    f.canvas.emit('click', { clientX: 0, clientY: 0 })
    assert.equal(f.picked.length, 1, 'a fresh click outside does not reuse a stale hovered node')
    f.step(240)
    assert.equal(f.frames.size, 0)
    const beforeFontLoad = f.metrics.measurements
    f.document.fonts.emit('loadingdone')
    f.step(3)
    assert.ok(f.metrics.measurements > beforeFontLoad, 'self-hosted font completion refits canvas labels')
    f.renderer.destroy()
})

function released(image) {
    return image.closed === true || (image.width === 0 && image.height === 0)
}

test('a cached card follows a node kind change even when its text and color stay the same', t => {
    const f = fixture({ bitmaps: true })
    t.after(() => f.renderer.destroy())
    f.renderer.setModel(model('shape', 0))
    f.step(240)
    const groupCard = f.renderer.cards.get('g:shape').image
    const updated = model('shape', 0)
    updated.nodes[1].kind = 'source'
    f.renderer.setModel(updated)
    f.step(3)
    assert.notEqual(f.renderer.cards.get('g:shape').image, groupCard, 'the former square status mark is replaced by a dot')
    assert.ok(released(groupCard), 'the obsolete card is released')
})

for (const bitmaps of [false, true]) {
    const backend = bitmaps ? 'ImageBitmap' : 'canvas fallback'

    test(`cached text follows model, font and theme changes; resize rebuilds at the new scale (${backend})`, () => {
        const f = fixture({ bitmaps })
        f.renderer.setModel(model('cached', 0))
        f.step(240)
        assert.equal(f.frames.size, 0)
        const initialFace = f.renderer.faces.get('x:cached').image
        const initialCard = f.renderer.cards.get('x:cached').image

        const updated = model('cached', 0)
        updated.nodes[2].title = 'Updated node'
        updated.nodes[2].sub = '48 ms'
        f.renderer.setModel(updated)
        f.step(3)
        assert.ok(released(initialFace) && released(initialCard), 'obsolete text and its composed card are released')
        assert.ok(f.texts.includes('Updated node') && f.texts.includes('48 ms'), 'fresh text is painted for the same node')

        const beforeFont = f.renderer.faces.get('x:cached').image
        const beforeFontMeasurements = f.metrics.measurements
        f.document.fonts.emit('loadingdone')
        assert.ok(released(beforeFont), 'font completion invalidates the cached text image')
        f.step(3)
        assert.ok(f.metrics.measurements > beforeFontMeasurements, 'font metrics are read again')

        const beforeTheme = f.renderer.cards.get('x:cached').image
        f.setDark(true)
        assert.ok(released(beforeTheme), 'old colors are not reused after the theme changes')
        f.step(3)
        assert.ok(f.metrics.blurs > 0, 'the refreshed dark sprites contain the status glows')

        const beforeResize = f.renderer.faces.get('x:cached').image
        f.window.devicePixelRatio = 2
        f.canvas.clientWidth = 450
        f.resizes[0].emit()
        f.step(240)
        assert.equal(f.canvas.width, 900)
        assert.equal(f.canvas.height, 500)
        assert.ok(released(beforeResize), 'old-size text sprites are released')
        assert.equal(f.renderer.faces.get('x:cached').image.height, 72, 'narrow text is painted at the new device scale')
        assert.equal(f.frames.size, 0, 'an idle resized graph settles again')
        f.renderer.destroy()
        assert.ok([...f.pictures, ...f.surfaces].every(released), 'all generated images are released')
    })

    test(`departed nodes release sprites; hiding releases link buffers; destroy releases the rest (${backend})`, () => {
        const f = fixture({ dark: true, bitmaps })
        f.renderer.setModel(model('departing'))
        f.step(120)
        const oldFace = f.renderer.faces.get('x:departing').image
        const oldCard = f.renderer.cards.get('x:departing').image
        f.renderer.setModel(model('remaining'))
        f.step(240)
        assert.ok(released(oldFace) && released(oldCard), 'outgoing node images live through their fade and are then released')
        assert.equal(f.renderer.faces.size, 3)
        assert.equal(f.renderer.cards.size, 3)
        const layers = f.renderer.layers.filter(Boolean)
        assert.equal(layers.length, 2, 'both link buffers were used')

        f.intersections[0].emit(false)
        assert.ok(layers.every(released), 'full-size link buffers are released while offscreen')
        assert.equal(f.frames.size, 0)
        f.renderer.setModel(model('hidden'))
        assert.equal(f.renderer.faces.size, 0, 'offscreen graph replacement releases the previous nodes without waiting for RAF')
        assert.equal(f.renderer.cards.size, 0)
        f.intersections[0].emit(true)
        f.step(120)
        assert.ok(f.renderer.particles.length > 0, 'new graph renders normally when shown again')
        assert.equal(f.renderer.faces.size, 3)
        f.renderer.destroy()
        f.renderer.destroy()
        assert.ok([...f.pictures, ...f.surfaces].every(released), 'destroy closes every bitmap or fallback canvas exactly once')
        assert.equal(f.frames.size, 0)
    })
}

test('destroy releases frames, observers, listeners and graph data; late work cannot restart it', () => {
    const f = fixture()
    f.renderer.setModel(model('dispose'))
    f.step(120)
    f.renderer.destroy()
    assert.equal(f.frames.size, 0)
    assert.equal(f.canvas.count(), 0)
    assert.equal(f.document.count(), 0)
    assert.equal(f.document.fonts.count(), 0)
    assert.equal(f.window.count(), 0)
    assert.equal([...f.media.values()].reduce((count, query) => count + query.count(), 0), 0)
    assert.equal(f.intersections[0].connected, false)
    assert.equal(f.resizes[0].connected, false)
    assert.equal(f.renderer.placed.size + f.renderer.links.size + f.renderer.stamps.size + f.renderer.particles.length, 0)
    f.renderer.setModel(model('late'))
    f.intersections[0].emit(true)
    f.resizes[0].emit()
    f.document.fonts.emit('loadingdone')
    assert.equal(f.frames.size, 0)
    assert.equal(f.renderer.placed.size, 0)
})
