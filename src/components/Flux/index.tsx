import classnames from 'classnames'
import { AnimatePresence, motion } from 'motion/react'
import { useId, useLayoutEffect, useRef } from 'react'

import { type BaseComponentProps } from '@models/BaseProps'

import { springs } from '../Motion'
import './style.css'

// Shared readings for the flux console: numbers that roll to their new value,
// names that slide when they change, rings and sparklines that morph.

const reduced = () => typeof window !== 'undefined' && window.matchMedia('(prefers-reduced-motion: reduce)').matches

const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB']

// Splits a byte count into a short number and its unit, e.g. [1.25, 'MiB'].
export function byteParts (bytes: number): [string, string] {
    if (!Number.isFinite(bytes) || bytes < 1) return ['0', 'B']
    const exp = Math.min(UNITS.length - 1, Math.floor(Math.log(bytes) / Math.log(1024)))
    const value = bytes / Math.pow(1024, exp)
    const digits = exp === 0 ? 0 : value < 10 ? 2 : value < 100 ? 1 : 0
    return [value.toFixed(digits), UNITS[exp]]
}

export function formatRate (bytesPerSecond: number) {
    if (!(bytesPerSecond >= 1)) return '0 B/s'
    const [n, unit] = byteParts(bytesPerSecond)
    return `${n} ${unit}/s`
}

// How good a latency is on a log scale, 0..1, for rings and bars.
export function latencyQuality (delay: number, failed = false) {
    if (failed) return 0.04
    if (!(delay > 0)) return 0
    return Math.max(0.08, Math.min(1, 1 - Math.log(delay / 50) / Math.log(1000 / 50)))
}

interface TweenNumberProps extends BaseComponentProps {
    value: number
    format?: (value: number) => string
    duration?: number
}

// A number that rolls from its previous value. The text is written straight to
// the DOM each frame, so a page of readings never re-renders while it rolls.
export function TweenNumber ({ value, format = v => String(Math.round(v)), duration = 700, className, style }: TweenNumberProps) {
    const ref = useRef<HTMLSpanElement>(null)
    const shown = useRef<number>()
    const formatRef = useRef(format)
    formatRef.current = format

    useLayoutEffect(() => {
        const element = ref.current
        if (element === null) return
        const from = shown.current
        shown.current = value
        // Jump when there is nothing sensible to roll from: first paint, failures, reduced motion.
        if (from === undefined || from === value || !(from > 0) || !(value > 0) || reduced()) {
            element.textContent = formatRef.current(value)
            return
        }
        let frame = 0
        const start = performance.now()
        const step = (now: number) => {
            const progress = Math.min(1, (now - start) / duration)
            const eased = 1 - Math.pow(1 - progress, 4)
            element.textContent = formatRef.current(from + (value - from) * eased)
            if (progress < 1) frame = requestAnimationFrame(step)
        }
        frame = requestAnimationFrame(step)
        return () => {
            cancelAnimationFrame(frame)
            element.textContent = formatRef.current(value)
        }
    }, [value, duration])

    return <span ref={ref} className={classnames('tween-number', className)} style={style} />
}

interface RollingBytesProps extends BaseComponentProps {
    value: number
    // append "/s" to the unit
    rate?: boolean
    duration?: number
}

// A byte count or rate that rolls to its new value, number and unit in separate spans.
export function RollingBytes ({ value, rate = false, duration = 800, className, style }: RollingBytesProps) {
    const number = useRef<HTMLSpanElement>(null)
    const unit = useRef<HTMLElement>(null)
    const shown = useRef<number>()

    useLayoutEffect(() => {
        const write = (v: number) => {
            const [n, u] = byteParts(v)
            if (number.current !== null) number.current.textContent = n
            if (unit.current !== null) unit.current.textContent = rate ? `${u}/s` : u
        }
        const from = shown.current
        shown.current = value
        if (from === undefined || from === value || reduced()) {
            write(value)
            return
        }
        let frame = 0
        const start = performance.now()
        const step = (now: number) => {
            const progress = Math.min(1, (now - start) / duration)
            const eased = 1 - Math.pow(1 - progress, 4)
            write(from + (value - from) * eased)
            if (progress < 1) frame = requestAnimationFrame(step)
        }
        frame = requestAnimationFrame(step)
        return () => {
            cancelAnimationFrame(frame)
            write(value)
        }
    }, [value, rate, duration])

    return (
        <span className={classnames('rolling-bytes', className)} style={style}>
            <span ref={number} className="rolling-bytes-number" />
            <small ref={unit} className="rolling-bytes-unit" />
        </span>
    )
}

interface SlideTextProps extends BaseComponentProps {
    text: string
    title?: string
}

// A name that slides up and out when it changes while its successor rises in.
export function SlideText ({ text, className, style, title }: SlideTextProps) {
    return (
        <span className={classnames('slide-text', className)} style={style} title={title ?? text}>
            <AnimatePresence initial={false} mode="popLayout">
                <motion.span
                    key={text}
                    className="slide-text-item"
                    initial={{ y: '100%', opacity: 0, filter: 'blur(3px)' }}
                    animate={{ y: '0%', opacity: 1, filter: 'blur(0px)' }}
                    exit={{ y: '-100%', opacity: 0, filter: 'blur(3px)' }}
                    transition={springs.glide}>
                    {text}
                </motion.span>
            </AnimatePresence>
        </span>
    )
}

interface SparklineProps extends BaseComponentProps {
    values: number[]
    max?: number
    width?: number
    height?: number
    tone?: 'accent' | 'accent-2' | 'good' | 'warn' | 'bad'
}

// Smooth line through the midpoints of the samples, plus a fading area beneath.
export function sparkPath (values: number[], width: number, height: number, max?: number, area = false) {
    const count = values.length
    if (count === 0) return `M0 ${height} L${width} ${height}`
    const top = max ?? Math.max(1, ...values) * 1.12
    const pad = 2
    const points = values.map((v, i) => [count === 1 ? width : (i / (count - 1)) * width, height - pad - (Math.max(0, Math.min(v, top)) / top) * (height - pad * 2)])
    let d = `M${points[0][0].toFixed(1)} ${points[0][1].toFixed(1)}`
    for (let i = 1; i < count; i++) {
        const [px, py] = points[i - 1]
        const [x, y] = points[i]
        d += ` Q${px.toFixed(1)} ${py.toFixed(1)} ${((px + x) / 2).toFixed(1)} ${((py + y) / 2).toFixed(1)}`
    }
    d += ` L${points[count - 1][0].toFixed(1)} ${points[count - 1][1].toFixed(1)}`
    if (area) d += ` L${width} ${height} L0 ${height} Z`
    return d
}

export function Sparkline ({ values, max, width = 100, height = 30, tone = 'accent', className, style }: SparklineProps) {
    const id = useId().replace(/:/g, '')
    const line = sparkPath(values, width, height, max)
    const area = sparkPath(values, width, height, max, true)
    return (
        <svg className={classnames('sparkline', `sparkline-${tone}`, className)} style={style} viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" aria-hidden="true">
            <defs>
                <linearGradient id={`spark-${id}`} x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0" className="sparkline-stop-top" />
                    <stop offset="1" className="sparkline-stop-bottom" />
                </linearGradient>
            </defs>
            <path className="sparkline-area" d={area} style={{ d: `path("${area}")` } as React.CSSProperties} fill={`url(#spark-${id})`} />
            <path className="sparkline-line" d={line} style={{ d: `path("${line}")` } as React.CSSProperties} fill="none" vectorEffect="non-scaling-stroke" />
        </svg>
    )
}

interface RingProps extends BaseComponentProps {
    // 0..1
    value: number
    size?: number
    stroke?: number
    tone?: 'flow' | 'good' | 'warn' | 'bad' | 'idle'
    children?: React.ReactNode
}

// A progress ring; the arc eases to its new length with a spring.
export function Ring ({ value, size = 66, stroke = 5, tone = 'flow', className, style, children }: RingProps) {
    const id = useId().replace(/:/g, '')
    const radius = (size - stroke) / 2 - 1
    const length = 2 * Math.PI * radius
    const clamped = Math.max(0, Math.min(1, value))
    return (
        <span className={classnames('ring', `ring-${tone}`, className)} style={{ width: size, height: size, ...style }}>
            <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} aria-hidden="true">
                <defs>
                    <linearGradient id={`ring-${id}`} x1="0" y1="0" x2="1" y2="1">
                        <stop offset="0" className="ring-stop-a" />
                        <stop offset="1" className="ring-stop-b" />
                    </linearGradient>
                </defs>
                <circle className="ring-track" cx={size / 2} cy={size / 2} r={radius} strokeWidth={stroke} />
                <circle
                    className="ring-value"
                    cx={size / 2}
                    cy={size / 2}
                    r={radius}
                    strokeWidth={stroke}
                    stroke={tone === 'flow' ? `url(#ring-${id})` : undefined}
                    strokeDasharray={length.toFixed(2)}
                    style={{ strokeDashoffset: (length * (1 - clamped)).toFixed(2) }} />
            </svg>
            {children != null && <span className="ring-center">{children}</span>}
        </span>
    )
}
