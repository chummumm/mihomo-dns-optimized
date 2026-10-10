// Canvas renderer for the live traffic map: sources on the left, policy groups in
// the middle, exits on the right. Links thicken with traffic and carry particles
// whose rate and speed follow it. Boxes ease to new places when the ranking changes.

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

const TAU = Math.PI * 2

function bezier (g: number[], t: number): [number, number] {
    const u = 1 - t
    return [
        u * u * u * g[0] + 3 * u * u * t * g[2] + 3 * u * t * t * g[4] + t * t * t * g[6],
        u * u * u * g[1] + 3 * u * u * t * g[3] + 3 * u * t * t * g[5] + t * t * t * g[7],
    ]
}

function ellipsize (ctx: CanvasRenderingContext2D, text: string, max: number) {
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
                this.placed.set(node.id, { node, x: NaN, y: NaN, alpha: 0, tx: 0, ty: 0, target: 1, pingAt: -Infinity })
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
            if (live === undefined) this.links.set(key, { ...link, alpha: 0, goal: 1, spawn: Math.random() })
            else Object.assign(live, link, { goal: 1 })
        }
        for (const [key, live] of this.links) {
            if (!keys.has(key)) live.goal = 0
        }
        this.onVisibility()
    }

    private readonly invalidate = () => {
        this.palette = null
        this.hues.clear()
        for (const placed of this.placed.values()) {
            placed.titleLabel = undefined
            placed.subLabel = undefined
        }
        this.schedule()
    }

    private readonly onResize = () => { this.schedule() }

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
        } else {
            this.schedule()
        }
    }

    private removeNode (id: string) {
        this.placed.delete(id)
        this.stamps.delete(id)
    }

    private label (placed: Placed, key: 'titleLabel' | 'subLabel', text: string, width: number) {
        const previous = placed[key]
        const font = this.ctx.font
        if (previous?.text === text && previous.font === font && previous.width === width) return previous.value
        const value = ellipsize(this.ctx, text, width)
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
        }
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
        const c = this.colors()
        const still = this.reduce.matches
        const dt = Math.min(0.05, (now - this.last) / 1000)
        this.last = now
        let moving = false

        const narrow = w < 620
        const pad = 2
        const top = 26
        this.boxW = narrow ? Math.max(78, (w - pad * 2 - 72) / 3) : Math.min(200, (w - pad * 2) / 4.3)
        this.boxH = narrow ? 36 : 42
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
            if (Math.abs(placed.tx - placed.x) > 0.3 || Math.abs(placed.ty - placed.y) > 0.3 || Math.abs(placed.target - placed.alpha) > 0.01) moving = true
            if (placed.target === 0 && placed.alpha < 0.02) this.removeNode(id)
        }

        const hover = this.hitTest(this.pointer)
        if (hover !== this.hover) {
            this.hover = hover
            moving = true
        }
        const hovered = hover !== null ? this.placed.get(hover) : undefined
        canvas.style.cursor = hovered?.node.target !== undefined ? 'pointer' : 'default'
        this.updateTooltip(hovered)
        const touches = (link: TopologyLink) => hover === null || link.from === hover || link.to === hover

        const geometry = (link: TopologyLink) => {
            const a = this.placed.get(link.from)
            const b = this.placed.get(link.to)
            if (a === undefined || b === undefined) return null
            const x1 = a.x + boxW
            const y1 = a.y + boxH / 2
            const x2 = b.x
            const y2 = b.y + boxH / 2
            const mx = (x1 + x2) / 2
            return [x1, y1, mx, y1, mx, y2, x2, y2]
        }

        ctx.clearRect(0, 0, w, h)
        ctx.font = '500 10px "Martian Mono", ui-monospace, monospace'
        ctx.fillStyle = c.faint
        ctx.textBaseline = 'middle'
        this.model.headings.forEach((heading, i) => ctx.fillText(heading, columnsX[i] + 2, 10))

        for (const [key, link] of this.links) {
            link.alpha += (link.goal - link.alpha) * (still ? 1 : Math.min(1, dt * 4))
            if (Math.abs(link.goal - link.alpha) > 0.01) moving = true
            if (link.goal === 0 && link.alpha < 0.02) {
                this.links.delete(key)
                continue
            }
            const g = geometry(link)
            if (g === null) continue
            const weight = link.rate > 0 ? Math.min(1, Math.log10(1 + link.rate / 2048) / 3.2) : 0
            ctx.save()
            ctx.globalAlpha = link.alpha * (touches(link) ? 1 : 0.12) * (0.32 + weight * 0.58)
            if (link.kind === 'proxy') {
                const gradient = ctx.createLinearGradient(g[0], 0, g[6], 0)
                gradient.addColorStop(0, c.accent)
                gradient.addColorStop(1, c.accent2)
                ctx.strokeStyle = gradient
            } else {
                ctx.strokeStyle = link.kind === 'reject' ? c.bad : c.faint
            }
            ctx.lineWidth = 1 + weight * 3.4
            if (link.rate <= 0) ctx.setLineDash([3, 4])
            ctx.beginPath()
            ctx.moveTo(g[0], g[1])
            ctx.bezierCurveTo(g[2], g[3], g[4], g[5], g[6], g[7])
            ctx.stroke()
            ctx.restore()
            if (!still && link.rate > 0 && link.kind !== 'reject') {
                link.spawn += dt * (0.5 + weight * 7.5)
                while (link.spawn > 1) {
                    link.spawn -= 1
                    this.particles.push({ link, t: 0, speed: 0.32 + Math.random() * 0.22 + weight * 0.35, radius: 1.1 + weight * 1.5 })
                }
            }
        }

        if (this.particles.length > 0) moving = true
        ctx.save()
        if (c.dark) ctx.globalCompositeOperation = 'lighter'
        for (let i = this.particles.length - 1; i >= 0; i--) {
            const p = this.particles[i]
            p.t += dt * p.speed
            const g = p.link.alpha > 0.02 ? geometry(p.link) : null
            if (p.t >= 1 || g === null) {
                this.particles.splice(i, 1)
                continue
            }
            const [x, y] = bezier(g, p.t)
            const color = p.link.kind === 'direct' ? c.dim : p.t < 0.5 ? c.accent : c.accent2
            const a = p.link.alpha * (touches(p.link) ? 1 : 0.1) * Math.sin(Math.PI * p.t)
            ctx.fillStyle = color
            ctx.globalAlpha = a * (c.dark ? 0.32 : 0.16)
            ctx.beginPath()
            ctx.arc(x, y, p.radius * (narrow ? 2.4 : 3.4), 0, TAU)
            ctx.fill()
            ctx.globalAlpha = a
            ctx.beginPath()
            ctx.arc(x, y, p.radius, 0, TAU)
            ctx.fill()
        }
        ctx.restore()
        if (this.particles.length > 800) this.particles.splice(0, this.particles.length - 800)

        for (const [id, placed] of this.placed) {
            const node = placed.node
            const hot = hover === id
            const dimmed = hover !== null && !hot && !this.linked(hover, id)
            ctx.save()
            ctx.globalAlpha = placed.alpha * (dimmed ? 0.45 : 1)
            ctx.fillStyle = c.panel
            ctx.strokeStyle = hot ? c.accent : c.line2
            ctx.lineWidth = 1
            if (c.dark && hot) {
                ctx.shadowColor = c.accent
                ctx.shadowBlur = 16
            }
            ctx.beginPath()
            if (typeof ctx.roundRect === 'function') ctx.roundRect(placed.x, placed.y, boxW, boxH, 10)
            else ctx.rect(placed.x, placed.y, boxW, boxH)
            ctx.fill()
            ctx.stroke()
            ctx.shadowBlur = 0

            const dotX = placed.x + (narrow ? 10 : 13)
            const dotY = placed.y + boxH / 2
            const dot = node.kind === 'group' ? this.hue(node.hue, c.accent) : this.toneColor(node.tone, c)
            if (node.kind === 'group') {
                ctx.fillStyle = dot
                ctx.beginPath()
                if (typeof ctx.roundRect === 'function') ctx.roundRect(dotX - 3.5, dotY - 3.5, 7, 7, 2)
                else ctx.rect(dotX - 3.5, dotY - 3.5, 7, 7)
                ctx.fill()
            } else {
                if (!still && now - placed.pingAt < 800) {
                    const k = (now - placed.pingAt) / 800
                    moving = true
                    ctx.save()
                    ctx.globalAlpha *= 1 - k
                    ctx.strokeStyle = dot
                    ctx.lineWidth = 2
                    ctx.beginPath()
                    ctx.arc(dotX, dotY, 4 + 14 * k, 0, TAU)
                    ctx.stroke()
                    ctx.restore()
                }
                if (c.dark) {
                    ctx.shadowColor = dot
                    ctx.shadowBlur = 8
                }
                ctx.fillStyle = dot
                ctx.beginPath()
                ctx.arc(dotX, dotY, node.kind === 'more' ? 2.4 : 3.2, 0, TAU)
                ctx.fill()
                ctx.shadowBlur = 0
            }

            const textX = placed.x + (narrow ? 20 : 25)
            const maxText = boxW - (narrow ? 25 : 33)
            ctx.textBaseline = 'alphabetic'
            ctx.fillStyle = node.kind === 'more' ? c.dim : c.text
            ctx.font = `600 ${narrow ? 10 : 12}px Sora, "PingFang SC", "Microsoft YaHei", sans-serif`
            ctx.fillText(this.label(placed, 'titleLabel', node.title, maxText), textX, placed.y + boxH / 2 - (narrow ? 1 : 2))
            ctx.fillStyle = c.dim
            ctx.font = `${narrow ? 8.5 : 9.5}px "Martian Mono", ui-monospace, monospace`
            ctx.fillText(this.label(placed, 'subLabel', node.sub, maxText), textX, placed.y + boxH / 2 + (narrow ? 11 : 13))
            ctx.restore()
        }

        // Particles keep flowing as long as any link carries traffic.
        if (!still) {
            for (const link of this.links.values()) {
                if (link.rate > 0 && link.alpha > 0.02 && link.kind !== 'reject') moving = true
            }
        }
        return moving
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
        tip.style.transform = `translate(${left.toFixed(0)}px, ${(placed.y + this.boxH + 6).toFixed(0)}px)`
    }
}
