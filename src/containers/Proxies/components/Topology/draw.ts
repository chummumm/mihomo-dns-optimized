// Canvas renderer for the live traffic map: sources on the left, policy groups in
// the middle, exits on the right. Links thicken with traffic and carry particles
// whose rate and speed follow it. Boxes ease to new places when the ranking changes.
//
// Every frame is built from parts that are cheap for the graphics card to stamp,
// so the motion stays as designed while far less is rendered per frame:
// - Headings and links are kept on an offscreen layer that a frame copies in one
//   go. It is repainted only when a link visibly changes: a box end moves by a
//   device pixel, an opacity moves by a colour step, new rates arrive, or the
//   pointer changes what is highlighted. Two buffers alternate, so repainting one
//   never waits on the copy of the other.
// - Particles are stamped from small pre-rendered sprites (halo and core) instead of
//   two path fills each, and still add light onto the links in dark.
// - Boxes are stamped from sprites: a resting box is one stamp of its card (plate,
//   dot with its glow, and text), redone only when its text changes. The hovered
//   box and boxes showing a test ping are stamped in parts so the glow and the ring
//   sit where they did. No shadow blur or text layout runs per frame.

export type TopologyKind = 'source' | 'group' | 'exit' | 'more'
export type TopologyTone = 'good' | 'warn' | 'bad' | 'idle' | 'live' | 'quiet'

export interface TopologyNode {
    id: string
    column: 0 | 1 | 2
    kind: TopologyKind
    title: string
    sub: string
    tone: TopologyTone
    // a CSS colour (for example var(--line-3)) for groups
    hue?: string
    // what a click opens, if anything
    target?: { type: 'group' | 'node', name: string }
    // changes whenever the exit gets a new test result
    stamp?: string
}

export interface TopologyLink {
    from: string
    to: string
    rate: number
    kind: 'proxy' | 'direct' | 'reject'
}

export interface TopologyModel {
    nodes: TopologyNode[]
    links: TopologyLink[]
    rows: number
    headings: [string, string, string]
}

interface Placed {
    node: TopologyNode
    x: number
    y: number
    alpha: number
    tx: number
    ty: number
    target: number
    pingAt: number
    // where the box is drawn, on whole device pixels so its sprites stay sharp
    sx: number
    sy: number
    // where the link layer last drew its ends
    lx: number
    ly: number
    titleLabel?: Label
    subLabel?: Label
}

interface Label {
    text: string
    font: string
    width: number
    value: string
}

interface LiveLink extends TopologyLink {
    alpha: number
    goal: number
    spawn: number
    // the curve this frame (start, two control points, end), or null while an end is missing
    curve: number[] | null
    // the opacity and weight it was last painted with
    drawn: number
    drawnWeight: number
}

interface Particle {
    link: LiveLink
    t: number
    speed: number
    radius: number
}

interface Palette {
    text: string
    dim: string
    faint: string
    line: string
    line2: string
    panel: string
    accent: string
    accent2: string
    good: string
    warn: string
    bad: string
    idle: string
    dark: boolean
}

type Context2D = CanvasRenderingContext2D | OffscreenCanvasRenderingContext2D
// A finished sprite: an ImageBitmap where the browser can make one, which it keeps
// on the graphics card and stamps cheaply; a canvas otherwise.
type Picture = ImageBitmap | HTMLCanvasElement

interface Sprite {
    key: string
    image: Picture
}

interface Plates {
    key: string
    plain: Picture
    hot: Picture
    // margins around the box in each sprite, in CSS pixels
    plainRoom: number
    hotRoom: number
}

const TAU = Math.PI * 2
// Room around a box plate for its hover glow, in CSS pixels.
const PLATE_ROOM = 24
// Room around a dot for its glow, in device pixels (canvas blur is not scaled).
const DOT_ROOM = 14
const PARTICLE_CAP = 800
// A link is repainted once its opacity has moved by a colour step.
const VISIBLE_STEP = 1 / 255

function bezier (g: number[], t: number): [number, number] {
    const u = 1 - t
    return [
        u * u * u * g[0] + 3 * u * u * t * g[2] + 3 * u * t * t * g[4] + t * t * t * g[6],
        u * u * u * g[1] + 3 * u * u * t * g[3] + 3 * u * t * t * g[5] + t * t * t * g[7],
    ]
}

function ellipsize (ctx: Context2D, text: string, max: number) {
    if (ctx.measureText(text).width <= max) return text
    // Fit long node names with logarithmic measurements, without splitting a surrogate pair.
    const characters = Array.from(text)
    let low = 0
    let high = characters.length
    while (low < high) {
        const middle = Math.ceil((low + high) / 2)
        if (ctx.measureText(characters.slice(0, middle).join('') + '…').width <= max) low = middle
        else high = middle - 1
    }
    return characters.slice(0, low).join('') + '…'
}

function luminance (color: string) {
    const hex = color.trim().match(/^#([0-9a-f]{6})$/i)
    if (hex === null) return 0
    const v = parseInt(hex[1], 16)
    return (0.2126 * (v >> 16) + 0.7152 * ((v >> 8) & 255) + 0.0722 * (v & 255)) / 255
}

function weightOf (rate: number) {
    return rate > 0 ? Math.min(1, Math.log10(1 + rate / 2048) / 3.2) : 0
}

function roundRect (ctx: Context2D, x: number, y: number, w: number, h: number, r: number) {
    ctx.beginPath()
    if (typeof ctx.roundRect === 'function') ctx.roundRect(x, y, w, h, r)
    else ctx.rect(x, y, w, h)
}

// A canvas of the given device size, reusing one when it is passed.
function surface (width: number, height: number, reuse?: HTMLCanvasElement) {
    const canvas = reuse ?? document.createElement('canvas')
    canvas.width = Math.max(1, Math.ceil(width))
    canvas.height = Math.max(1, Math.ceil(height))
    return canvas
}

// Paints a sprite of the given device size once and returns it ready to stamp.
function paint (width: number, height: number, draw: (ctx: Context2D) => void): Picture {
    const w = Math.max(1, Math.ceil(width))
    const h = Math.max(1, Math.ceil(height))
    if (typeof OffscreenCanvas === 'function') {
        const offscreen = new OffscreenCanvas(w, h)
        const ctx = offscreen.getContext('2d')
        if (ctx !== null && typeof offscreen.transferToImageBitmap === 'function') {
            draw(ctx)
            return offscreen.transferToImageBitmap()
        }
    }
    const canvas = surface(w, h)
    draw(canvas.getContext('2d')!)
    return canvas
}

function release (picture: Picture | undefined) {
    if (picture === undefined) return
    if ('close' in picture) {
        picture.close()
    } else {
        picture.width = 0
        picture.height = 0
    }
}

export interface TopologyCallbacks {
    onPick?: (target: NonNullable<TopologyNode['target']>) => void
}

export class TopologyRenderer {
    private readonly ctx: CanvasRenderingContext2D
    private model: TopologyModel = { nodes: [], links: [], rows: 1, headings: ['', '', ''] }
    private readonly placed = new Map<string, Placed>()
    private readonly links = new Map<string, LiveLink>()
    private readonly stamps = new Map<string, string>()
    private readonly particles: Particle[] = []
    private palette: Palette | null = null
    private readonly hues = new Map<string, string>()
    private pointer: [number, number] | null = null
    private hover: string | null = null
    private frame = 0
    private last = 0
    private visible = true
    private running = false
    private destroyed = false
    private readonly reduce = window.matchMedia('(prefers-reduced-motion: reduce)')
    private readonly scheme = window.matchMedia('(prefers-color-scheme: dark)')
    private readonly observer: IntersectionObserver | undefined
    private readonly resizeObserver: ResizeObserver | undefined
    private boxW = 180
    private boxH = 42
    private narrow = false
    // the two offscreen buffers holding headings and links, and which one is shown
    private readonly layers: Array<HTMLCanvasElement | undefined> = [undefined, undefined]
    private front = 0
    private rebuild = true
    private headings: Sprite | undefined
    private plates: Plates | undefined
    private readonly dots = new Map<string, Picture>()
    private readonly faces = new Map<string, Sprite>()
    private readonly cards = new Map<string, Sprite>()
    private readonly sparks = new Map<string, Picture>()
    private cursor = ''
    private tipAt = ''

    constructor (private readonly canvas: HTMLCanvasElement, private readonly tooltip: HTMLElement | null, private readonly callbacks: TopologyCallbacks = {}) {
        this.ctx = canvas.getContext('2d')!
        canvas.addEventListener('pointermove', this.onMove)
        canvas.addEventListener('pointerleave', this.onLeave)
        canvas.addEventListener('click', this.onClick)
        this.scheme.addEventListener('change', this.invalidate)
        this.reduce.addEventListener('change', this.onMotionChange)
        document.addEventListener('visibilitychange', this.onVisibility)
        document.fonts?.addEventListener('loadingdone', this.invalidate)
        if (typeof IntersectionObserver === 'function') {
            this.observer = new IntersectionObserver(entries => {
                this.visible = entries.some(entry => entry.isIntersecting)
                this.onVisibility()
            })
            this.observer.observe(canvas)
        }
        if (typeof ResizeObserver === 'function') {
            this.resizeObserver = new ResizeObserver(this.onResize)
            this.resizeObserver.observe(canvas)
        } else {
            window.addEventListener('resize', this.onResize)
        }
        this.schedule()
    }

    destroy () {
        this.destroyed = true
        cancelAnimationFrame(this.frame)
        this.running = false
        this.canvas.removeEventListener('pointermove', this.onMove)
        this.canvas.removeEventListener('pointerleave', this.onLeave)
        this.canvas.removeEventListener('click', this.onClick)
        this.scheme.removeEventListener('change', this.invalidate)
        this.reduce.removeEventListener('change', this.onMotionChange)
        document.removeEventListener('visibilitychange', this.onVisibility)
        document.fonts?.removeEventListener('loadingdone', this.invalidate)
        this.observer?.disconnect()
        this.resizeObserver?.disconnect()
        window.removeEventListener('resize', this.onResize)
        this.placed.clear()
        this.links.clear()
        this.stamps.clear()
        this.hues.clear()
        this.particles.length = 0
        this.model = { nodes: [], links: [], rows: 1, headings: ['', '', ''] }
        this.dropSprites()
        release(this.layers[0])
        release(this.layers[1])
        this.layers[0] = this.layers[1] = undefined
        this.updateTooltip(undefined)
    }

    setModel (model: TopologyModel) {
        if (this.destroyed) return
        this.model = model
        const now = performance.now()
        const wanted = new Set(model.nodes.map(node => node.id))
        for (const node of model.nodes) {
            const placed = this.placed.get(node.id)
            if (placed === undefined) {
                this.placed.set(node.id, { node, x: NaN, y: NaN, alpha: 0, tx: 0, ty: 0, target: 1, pingAt: -Infinity, sx: NaN, sy: NaN, lx: NaN, ly: NaN })
            } else {
                placed.node = node
                placed.target = 1
            }
            if (node.stamp !== undefined) {
                const before = this.stamps.get(node.id)
                if (before !== undefined && before !== node.stamp) this.placed.get(node.id)!.pingAt = now
                this.stamps.set(node.id, node.stamp)
            } else {
                this.stamps.delete(node.id)
            }
        }
        for (const [id, placed] of this.placed) {
            if (!wanted.has(id)) placed.target = 0
        }
        const keys = new Set<string>()
        for (const link of model.links) {
            const key = `${link.from}>${link.to}`
            keys.add(key)
            const live = this.links.get(key)
            if (live === undefined) this.links.set(key, { ...link, alpha: 0, goal: 1, spawn: Math.random(), curve: null, drawn: -1, drawnWeight: -1 })
            else Object.assign(live, link, { goal: 1 })
        }
        for (const [key, live] of this.links) {
            if (!keys.has(key)) live.goal = 0
        }
        // New rates and headings: the link layer is repainted on the next frame.
        this.rebuild = true
        this.onVisibility()
    }

    private readonly invalidate = () => {
        this.palette = null
        this.hues.clear()
        for (const placed of this.placed.values()) {
            placed.titleLabel = undefined
            placed.subLabel = undefined
        }
        this.dropSprites()
        this.rebuild = true
        this.schedule()
    }

    private dropSprites () {
        release(this.headings?.image)
        release(this.plates?.plain)
        release(this.plates?.hot)
        for (const image of this.dots.values()) release(image)
        for (const face of this.faces.values()) release(face.image)
        for (const card of this.cards.values()) release(card.image)
        for (const image of this.sparks.values()) release(image)
        this.headings = undefined
        this.plates = undefined
        this.dots.clear()
        this.faces.clear()
        this.cards.clear()
        this.sparks.clear()
    }

    private readonly onResize = () => {
        this.rebuild = true
        this.schedule()
    }

    private readonly onMotionChange = () => {
        if (this.reduce.matches) this.particles.length = 0
        this.schedule()
    }

    private readonly onVisibility = () => {
        if (!this.visible || document.hidden) {
            cancelAnimationFrame(this.frame)
            this.running = false
            this.particles.length = 0
            this.pointer = null
            this.hover = null
            this.updateTooltip(undefined)
            // There is no visible exit to animate. Reconcile now instead of
            // retaining every historical source until drawing resumes.
            for (const [id, placed] of this.placed) {
                if (placed.target === 0) this.removeNode(id)
                else {
                    placed.alpha = 1
                    placed.x = NaN
                    placed.pingAt = -Infinity
                }
            }
            for (const [key, link] of this.links) {
                if (link.goal === 0) this.links.delete(key)
                else link.alpha = 1
            }
            // The link buffers are full-size; nobody sees them while the map is away.
            release(this.layers[0])
            release(this.layers[1])
            this.layers[0] = this.layers[1] = undefined
            this.rebuild = true
        } else {
            this.schedule()
        }
    }

    private removeNode (id: string) {
        this.placed.delete(id)
        this.stamps.delete(id)
        release(this.faces.get(id)?.image)
        release(this.cards.get(id)?.image)
        this.cards.delete(id)
        this.faces.delete(id)
    }

    private label (ctx: Context2D, placed: Placed, key: 'titleLabel' | 'subLabel', text: string, width: number) {
        const previous = placed[key]
        const font = ctx.font
        if (previous?.text === text && previous.font === font && previous.width === width) return previous.value
        const value = ellipsize(ctx, text, width)
        placed[key] = { text, font, width, value }
        return value
    }

    private readonly onMove = (event: PointerEvent) => {
        const rect = this.canvas.getBoundingClientRect()
        this.pointer = [event.clientX - rect.left, event.clientY - rect.top]
        this.schedule()
    }

    private readonly onLeave = () => {
        this.pointer = null
        this.schedule()
    }

    private readonly onClick = (event: MouseEvent) => {
        const rect = this.canvas.getBoundingClientRect()
        const id = this.hitTest([event.clientX - rect.left, event.clientY - rect.top])
        const target = id === null ? undefined : this.placed.get(id)?.node.target
        if (target !== undefined) this.callbacks.onPick?.(target)
    }

    private hitTest (point: [number, number] | null) {
        if (point === null) return null
        let hit: string | null = null
        for (const [id, placed] of this.placed) {
            if (placed.target === 0) continue
            if (point[0] >= placed.x && point[0] <= placed.x + this.boxW && point[1] >= placed.y && point[1] <= placed.y + this.boxH) hit = id
        }
        return hit
    }

    private schedule () {
        if (this.destroyed || this.running || !this.visible || document.hidden) return
        this.running = true
        this.last = performance.now()
        this.frame = requestAnimationFrame(this.tick)
    }

    private readonly tick = (now: number) => {
        if (this.destroyed || !this.visible || document.hidden || !this.canvas.isConnected) {
            this.running = false
            return
        }
        const busy = this.draw(now)
        if (busy) {
            this.frame = requestAnimationFrame(this.tick)
        } else {
            this.running = false
        }
    }

    private colors (): Palette {
        if (this.palette !== null) return this.palette
        const style = getComputedStyle(this.canvas)
        const read = (name: string) => style.getPropertyValue(name).trim()
        const panel = read('--panel-solid')
        this.palette = {
            text: read('--text'),
            dim: read('--text-3'),
            faint: read('--faint'),
            line: read('--hair'),
            line2: read('--hair-2'),
            panel,
            accent: read('--accent'),
            accent2: read('--accent-2'),
            good: read('--good'),
            warn: read('--warn'),
            bad: read('--bad'),
            idle: read('--idle'),
            dark: luminance(panel) < 0.45,
        }
        return this.palette
    }

    private hue (value: string | undefined, fallback: string) {
        if (value === undefined) return fallback
        const cached = this.hues.get(value)
        if (cached !== undefined) return cached
        const name = value.match(/var\((--[\w-]+)\)/)?.[1]
        const resolved = name !== undefined ? getComputedStyle(this.canvas).getPropertyValue(name).trim() || fallback : value
        this.hues.set(value, resolved)
        return resolved
    }

    private toneColor (tone: TopologyTone, c: Palette) {
        switch (tone) {
            case 'good': return c.good
            case 'warn': return c.warn
            case 'bad': return c.bad
            case 'live': return c.accent
            case 'quiet': return c.faint
            default: return c.idle
        }
    }

    private dotColor (node: TopologyNode, c: Palette) {
        return node.kind === 'group' ? this.hue(node.hue, c.accent) : this.toneColor(node.tone, c)
    }

    // Returns whether anything is still moving and needs another frame.
    private draw (now: number) {
        const canvas = this.canvas
        const ctx = this.ctx
        const w = canvas.clientWidth
        const h = canvas.clientHeight
        if (w === 0 || h === 0) return false
        const dpr = Math.min(2, window.devicePixelRatio || 1)
        if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
            canvas.width = Math.round(w * dpr)
            canvas.height = Math.round(h * dpr)
            this.rebuild = true
        }
        const c = this.colors()
        const still = this.reduce.matches
        const dt = Math.max(0, Math.min(0.05, (now - this.last) / 1000))
        this.last = now
        let moving = false

        const narrow = w < 620
        const pad = 2
        const top = 26
        this.boxW = narrow ? Math.max(78, (w - pad * 2 - 72) / 3) : Math.min(200, (w - pad * 2) / 4.3)
        this.boxH = narrow ? 36 : 42
        this.narrow = narrow
        const boxW = this.boxW
        const boxH = this.boxH
        const columnsX = [pad, (w - boxW) / 2, w - pad - boxW]
        const counts = [0, 0, 0]
        const index = new Map<string, number>()
        for (const node of this.model.nodes) index.set(node.id, counts[node.column]++)
        const ease = still ? 1 : Math.min(1, dt * 9)

        // Place boxes; each eases toward its slot and fades in or out.
        for (const [id, placed] of this.placed) {
            const i = index.get(id)
            if (i !== undefined) {
                const n = counts[placed.node.column]
                placed.tx = columnsX[placed.node.column]
                placed.ty = top + ((h - top - 4) * (i + 0.5)) / n - boxH / 2
            }
            if (Number.isNaN(placed.x)) {
                placed.x = placed.tx
                placed.y = placed.ty
            }
            placed.x += (placed.tx - placed.x) * ease
            placed.y += (placed.ty - placed.y) * ease
            placed.alpha += (placed.target - placed.alpha) * (still ? 1 : Math.min(1, dt * 6))
            // Within a fraction of a pixel (or of opacity) a box lands, so at rest it is exactly still.
            if (Math.abs(placed.tx - placed.x) <= 0.3) placed.x = placed.tx
            if (Math.abs(placed.ty - placed.y) <= 0.3) placed.y = placed.ty
            if (Math.abs(placed.target - placed.alpha) <= 0.01) placed.alpha = placed.target
            if (placed.x !== placed.tx || placed.y !== placed.ty || placed.alpha !== placed.target) moving = true
            placed.sx = Math.round(placed.x * dpr) / dpr
            placed.sy = Math.round(placed.y * dpr) / dpr
            if (placed.sx !== placed.lx || placed.sy !== placed.ly) this.rebuild = true
            if (placed.target === 0 && placed.alpha < 0.02) {
                this.removeNode(id)
                this.rebuild = true
            }
        }

        const hover = this.hitTest(this.pointer)
        if (hover !== this.hover) {
            this.hover = hover
            this.rebuild = true
            moving = true
        }
        const hovered = hover !== null ? this.placed.get(hover) : undefined
        const cursor = hovered?.node.target !== undefined ? 'pointer' : 'default'
        if (cursor !== this.cursor) {
            this.cursor = cursor
            canvas.style.cursor = cursor
        }
        this.updateTooltip(hovered)
        const touches = (link: TopologyLink) => hover === null || link.from === hover || link.to === hover

        for (const [key, link] of this.links) {
            link.alpha += (link.goal - link.alpha) * (still ? 1 : Math.min(1, dt * 4))
            if (Math.abs(link.goal - link.alpha) <= 0.01) link.alpha = link.goal
            if (link.alpha !== link.goal) moving = true
            if (link.goal === 0 && link.alpha < 0.02) {
                this.links.delete(key)
                this.rebuild = true
                continue
            }
            link.curve = this.curve(link)
            const weight = weightOf(link.rate)
            if (weight !== link.drawnWeight || Math.abs(this.shown(link, touches) - link.drawn) >= VISIBLE_STEP) this.rebuild = true
            if (!still && link.curve !== null && link.rate > 0 && link.kind !== 'reject') {
                link.spawn += dt * (0.5 + weight * 7.5)
                while (link.spawn > 1) {
                    link.spawn -= 1
                    this.particles.push({ link, t: 0, speed: 0.32 + Math.random() * 0.22 + weight * 0.35, radius: 1.1 + weight * 1.5 })
                }
            }
        }

        const shown = this.layers[this.front]
        if (this.rebuild || shown?.width !== canvas.width || shown.height !== canvas.height) this.paintLinks(c, dpr, columnsX, touches)

        // The frame: the link layer, particles adding light onto it, then the boxes.
        ctx.setTransform(1, 0, 0, 1, 0, 0)
        ctx.globalAlpha = 1
        ctx.globalCompositeOperation = 'source-over'
        ctx.clearRect(0, 0, canvas.width, canvas.height)
        const layer = this.layers[this.front]
        if (layer !== undefined) ctx.drawImage(layer, 0, 0)
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0)

        if (this.particles.length > 0) moving = true
        if (c.dark) ctx.globalCompositeOperation = 'lighter'
        const ratio = narrow ? 2.4 : 3.4
        let kept = 0
        for (const p of this.particles) {
            p.t += dt * p.speed
            const g = p.link.alpha > 0.02 ? p.link.curve : null
            if (p.t >= 1 || g === null) continue
            this.particles[kept++] = p
            const [x, y] = bezier(g, p.t)
            const color = p.link.kind === 'direct' ? c.dim : p.t < 0.5 ? c.accent : c.accent2
            const a = p.link.alpha * (touches(p.link) ? 1 : 0.1) * Math.sin(Math.PI * p.t)
            if (a <= 0) continue
            const spark = this.spark(color, p.radius, ratio, c, dpr)
            const size = spark.width / dpr
            ctx.globalAlpha = a
            ctx.drawImage(spark, x - size / 2, y - size / 2, size, size)
        }
        this.particles.length = kept
        if (kept > PARTICLE_CAP) this.particles.splice(0, kept - PARTICLE_CAP)
        ctx.globalCompositeOperation = 'source-over'

        const plates = this.platesFor(c, dpr)
        for (const [id, placed] of this.placed) {
            const node = placed.node
            const hot = hover === id
            const dimmed = hover !== null && !hot && !this.linked(hover, id)
            const alpha = placed.alpha * (dimmed ? 0.45 : 1)
            if (alpha <= 0) continue
            ctx.globalAlpha = alpha
            const pinging = node.kind !== 'group' && !still && now - placed.pingAt < 800
            if (!hot && !pinging) {
                const card = this.cardFor(placed, plates, c, dpr)
                ctx.drawImage(card, placed.sx - plates.plainRoom, placed.sy - plates.plainRoom, card.width / dpr, card.height / dpr)
                continue
            }
            const plate = hot ? plates.hot : plates.plain
            const room = hot ? plates.hotRoom : plates.plainRoom
            ctx.drawImage(plate, placed.sx - room, placed.sy - room, plate.width / dpr, plate.height / dpr)

            const dotX = placed.sx + (narrow ? 10 : 13)
            const dotY = placed.sy + boxH / 2
            const dot = this.dotColor(node, c)
            if (pinging) {
                const k = (now - placed.pingAt) / 800
                moving = true
                ctx.globalAlpha = alpha * (1 - k)
                ctx.strokeStyle = dot
                ctx.lineWidth = 2
                ctx.beginPath()
                ctx.arc(dotX, dotY, 4 + 14 * k, 0, TAU)
                ctx.stroke()
                ctx.globalAlpha = alpha
            }
            const mark = this.dotFor(node.kind, dot, c, dpr)
            const half = mark.width / dpr / 2
            ctx.drawImage(mark, dotX - half, dotY - half, mark.width / dpr, mark.height / dpr)
            const face = this.faceFor(placed, c, dpr)
            ctx.drawImage(face, placed.sx, placed.sy, face.width / dpr, face.height / dpr)
        }
        ctx.globalAlpha = 1

        // Particles keep flowing as long as any link carries traffic.
        if (!still) {
            for (const link of this.links.values()) {
                if (link.rate > 0 && link.alpha > 0.02 && link.kind !== 'reject') moving = true
            }
        }
        return moving
    }

    private curve (link: LiveLink) {
        const a = this.placed.get(link.from)
        const b = this.placed.get(link.to)
        if (a === undefined || b === undefined) return null
        const x1 = a.sx + this.boxW
        const y1 = a.sy + this.boxH / 2
        const x2 = b.sx
        const y2 = b.sy + this.boxH / 2
        const mx = (x1 + x2) / 2
        const g = link.curve ?? new Array<number>(8)
        g[0] = x1
        g[1] = y1
        g[2] = mx
        g[3] = y1
        g[4] = mx
        g[5] = y2
        g[6] = x2
        g[7] = y2
        return g
    }

    private shown (link: LiveLink, touches: (link: TopologyLink) => boolean) {
        return link.alpha * (touches(link) ? 1 : 0.12) * (0.32 + weightOf(link.rate) * 0.58)
    }

    // Headings and links into the back buffer, which then becomes the one shown.
    private paintLinks (c: Palette, dpr: number, columnsX: number[], touches: (link: TopologyLink) => boolean) {
        const width = this.canvas.width
        const height = this.canvas.height
        const back = 1 - this.front
        const existing = this.layers[back]
        const layer = existing !== undefined && existing.width === width && existing.height === height ? existing : surface(width, height, existing)
        this.layers[back] = layer
        const ctx = layer.getContext('2d')!
        ctx.setTransform(1, 0, 0, 1, 0, 0)
        ctx.globalAlpha = 1
        ctx.clearRect(0, 0, width, height)
        ctx.drawImage(this.headingsFor(c, dpr, columnsX), 0, 0)
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
        for (const link of this.links.values()) {
            const weight = weightOf(link.rate)
            const shown = this.shown(link, touches)
            link.drawn = shown
            link.drawnWeight = weight
            const g = link.curve
            if (g === null) continue
            ctx.globalAlpha = shown
            if (link.kind === 'proxy') {
                const gradient = ctx.createLinearGradient(g[0], 0, g[6], 0)
                gradient.addColorStop(0, c.accent)
                gradient.addColorStop(1, c.accent2)
                ctx.strokeStyle = gradient
            } else {
                ctx.strokeStyle = link.kind === 'reject' ? c.bad : c.faint
            }
            ctx.lineWidth = 1 + weight * 3.4
            ctx.setLineDash(link.rate <= 0 ? [3, 4] : [])
            ctx.beginPath()
            ctx.moveTo(g[0], g[1])
            ctx.bezierCurveTo(g[2], g[3], g[4], g[5], g[6], g[7])
            ctx.stroke()
        }
        ctx.setLineDash([])
        ctx.globalAlpha = 1
        for (const placed of this.placed.values()) {
            placed.lx = placed.sx
            placed.ly = placed.sy
        }
        this.front = back
        this.rebuild = false
    }

    // The three column headings, redone only when the width or the language changes.
    private headingsFor (c: Palette, dpr: number, columnsX: number[]) {
        const key = `${this.model.headings.join('|')}|${columnsX.join('|')}|${this.canvas.width}|${dpr}`
        if (this.headings?.key === key) return this.headings.image
        release(this.headings?.image)
        const image = paint(this.canvas.width, 20 * dpr, ctx => {
            ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
            ctx.font = '500 10px "Martian Mono", ui-monospace, monospace'
            ctx.fillStyle = c.faint
            ctx.textBaseline = 'middle'
            this.model.headings.forEach((heading, i) => ctx.fillText(heading, columnsX[i] + 2, 10))
        })
        this.headings = { key, image }
        return image
    }

    // The box plate, plain and under the pointer, shared by every box.
    private platesFor (c: Palette, dpr: number): Plates {
        const key = `${this.boxW}|${this.boxH}|${dpr}`
        if (this.plates?.key === key) return this.plates
        release(this.plates?.plain)
        release(this.plates?.hot)
        // Only the hovered plate glows, so only it carries room for the glow; the
        // plain one is cut tight to keep each stamp small.
        const make = (hot: boolean, margin: number) => paint(Math.ceil(this.boxW * dpr) + margin * 2, Math.ceil(this.boxH * dpr) + margin * 2, ctx => {
            ctx.setTransform(dpr, 0, 0, dpr, margin, margin)
            ctx.fillStyle = c.panel
            ctx.strokeStyle = hot ? c.accent : c.line2
            ctx.lineWidth = 1
            if (c.dark && hot) {
                ctx.shadowColor = c.accent
                ctx.shadowBlur = 16
            }
            roundRect(ctx, 0, 0, this.boxW, this.boxH, 10)
            ctx.fill()
            ctx.stroke()
        })
        const plainMargin = Math.ceil(dpr)
        const hotMargin = Math.ceil(PLATE_ROOM * dpr)
        this.plates = { key, plain: make(false, plainMargin), hot: make(true, hotMargin), plainRoom: plainMargin / dpr, hotRoom: hotMargin / dpr }
        return this.plates
    }

    // A box's status mark, with its glow in dark; one per colour and shape.
    private dotFor (kind: TopologyKind, color: string, c: Palette, dpr: number) {
        const shape = kind === 'group' ? 'square' : kind === 'more' ? 'small' : 'dot'
        const key = `${shape}|${color}|${dpr}`
        const cached = this.dots.get(key)
        if (cached !== undefined) return cached
        const glow = c.dark && shape !== 'square'
        const size = Math.ceil(8 * dpr) + (glow ? DOT_ROOM : 1) * 2
        const image = paint(size, size, ctx => {
            ctx.setTransform(dpr, 0, 0, dpr, size / 2, size / 2)
            ctx.fillStyle = color
            if (shape === 'square') {
                roundRect(ctx, -3.5, -3.5, 7, 7, 2)
                ctx.fill()
            } else {
                if (glow) {
                    ctx.shadowColor = color
                    ctx.shadowBlur = 8
                }
                ctx.beginPath()
                ctx.arc(0, 0, shape === 'small' ? 2.4 : 3.2, 0, TAU)
                ctx.fill()
            }
        })
        this.dots.set(key, image)
        return image
    }

    // A box's title and reading, redone only when they change.
    private faceFor (placed: Placed, c: Palette, dpr: number) {
        const node = placed.node
        const { boxW, boxH, narrow } = this
        const key = `${node.kind === 'more' ? 1 : 0}|${node.title}|${node.sub}|${narrow ? 1 : 0}|${boxW}|${boxH}|${dpr}`
        const cached = this.faces.get(node.id)
        if (cached?.key === key) return cached.image
        release(cached?.image)
        const textX = narrow ? 20 : 25
        const maxText = boxW - (narrow ? 25 : 33)
        const image = paint(boxW * dpr, boxH * dpr, ctx => {
            ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
            ctx.textBaseline = 'alphabetic'
            ctx.fillStyle = node.kind === 'more' ? c.dim : c.text
            ctx.font = `600 ${narrow ? 10 : 12}px Sora, "PingFang SC", "Microsoft YaHei", sans-serif`
            ctx.fillText(this.label(ctx, placed, 'titleLabel', node.title, maxText), textX, boxH / 2 - (narrow ? 1 : 2))
            ctx.fillStyle = c.dim
            ctx.font = `${narrow ? 8.5 : 9.5}px "Martian Mono", ui-monospace, monospace`
            ctx.fillText(this.label(ctx, placed, 'subLabel', node.sub, maxText), textX, boxH / 2 + (narrow ? 11 : 13))
        })
        this.faces.set(node.id, { key, image })
        return image
    }

    // A resting box in one stamp: its plate, its dot and its text.
    private cardFor (placed: Placed, plates: Plates, c: Palette, dpr: number) {
        const node = placed.node
        const face = this.faceFor(placed, c, dpr)
        const dot = this.dotColor(node, c)
        const mark = this.dotFor(node.kind, dot, c, dpr)
        const faceKey = this.faces.get(node.id)?.key ?? ''
        const key = `${node.kind}|${faceKey}|${dot}|${plates.key}|${c.dark ? 1 : 0}`
        const cached = this.cards.get(node.id)
        if (cached?.key === key) return cached.image
        release(cached?.image)
        const plate = plates.plain
        const margin = plates.plainRoom * dpr
        const centerX = margin + (this.narrow ? 10 : 13) * dpr
        const centerY = margin + (this.boxH / 2) * dpr
        const image = paint(plate.width, plate.height, ctx => {
            ctx.drawImage(plate, 0, 0)
            ctx.drawImage(mark, centerX - mark.width / 2, centerY - mark.height / 2)
            ctx.drawImage(face, margin, margin)
        })
        this.cards.set(node.id, { key, image })
        return image
    }

    // A particle: a soft halo around a bright core, one sprite per colour and
    // quarter-pixel step of radius.
    private spark (color: string, radius: number, ratio: number, c: Palette, dpr: number) {
        const r = Math.max(0.5, Math.round(radius * 4) / 4)
        const key = `${color}|${r}|${ratio}|${dpr}`
        const cached = this.sparks.get(key)
        if (cached !== undefined) return cached
        const halo = r * ratio
        const size = Math.ceil((halo * 2 + 2) * dpr)
        const image = paint(size, size, ctx => {
            ctx.setTransform(dpr, 0, 0, dpr, size / 2, size / 2)
            ctx.fillStyle = color
            ctx.globalAlpha = c.dark ? 0.32 : 0.16
            ctx.beginPath()
            ctx.arc(0, 0, halo, 0, TAU)
            ctx.fill()
            if (c.dark) ctx.globalCompositeOperation = 'lighter'
            ctx.globalAlpha = 1
            ctx.beginPath()
            ctx.arc(0, 0, r, 0, TAU)
            ctx.fill()
        })
        this.sparks.set(key, image)
        return image
    }

    private linked (a: string, b: string) {
        for (const link of this.links.values()) {
            if ((link.from === a && link.to === b) || (link.from === b && link.to === a)) return true
        }
        return false
    }

    private updateTooltip (placed: Placed | undefined) {
        const tip = this.tooltip
        if (tip === null) return
        if (placed === undefined || this.pointer === null) {
            if (tip.dataset.for !== '') {
                tip.dataset.for = ''
                tip.classList.remove('is-shown')
            }
            return
        }
        if (tip.dataset.for !== placed.node.id) {
            tip.dataset.for = placed.node.id
            tip.classList.add('is-shown')
        }
        const title = tip.firstElementChild as HTMLElement | null
        const sub = tip.lastElementChild as HTMLElement | null
        if (title !== null && title.textContent !== placed.node.title) title.textContent = placed.node.title
        if (sub !== null && sub.textContent !== placed.node.sub) sub.textContent = placed.node.sub
        const width = this.canvas.clientWidth
        const left = Math.min(Math.max(0, placed.x), Math.max(0, width - tip.offsetWidth))
        const at = `translate(${left.toFixed(0)}px, ${(placed.y + this.boxH + 6).toFixed(0)}px)`
        if (at !== this.tipAt || tip.style.transform !== at) {
            this.tipAt = at
            tip.style.transform = at
        }
    }
}
