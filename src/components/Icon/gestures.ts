// Click gestures: each icon acts out what its control does, once, on the icon's wrapper.
// Hover poses live in style.css and move the inner svg, so the two layers never fight.

interface Gesture {
    frames: Keyframe[]
    duration: number
    origin?: string
    easing?: string
}

const OUT = 'cubic-bezier(0.22, 1, 0.36, 1)'

const turn = (deg: number): Gesture => ({
    frames: [{ transform: 'rotate(0deg)' }, { transform: `rotate(${deg}deg)` }],
    duration: 640,
    easing: 'spring',
})

const nudge = (x: number, y: number): Gesture => ({
    frames: [
        { transform: 'translate(0, 0)' },
        { transform: `translate(${x}px, ${y}px)`, offset: 0.35 },
        { transform: 'translate(0, 0)' },
    ],
    duration: 420,
})

const gestures: Record<string, Gesture> = {
    // a full turn of the refresh arrows
    update: turn(360),
    // the bolt strikes: lifts, drops past rest, settles
    speed: {
        frames: [
            { transform: 'translateY(0) scale(1) rotate(0deg)' },
            { transform: 'translateY(-3px) scale(1.22) rotate(-10deg)', offset: 0.3 },
            { transform: 'translateY(1.5px) scale(0.9) rotate(4deg)', offset: 0.62 },
            { transform: 'translateY(0) scale(1) rotate(0deg)' },
        ],
        duration: 560,
    },
    // two beats
    healthcheck: {
        frames: [
            { transform: 'scale(1)' },
            { transform: 'scale(1.24)', offset: 0.18 },
            { transform: 'scale(0.96)', offset: 0.38 },
            { transform: 'scale(1.14)', offset: 0.56 },
            { transform: 'scale(1)' },
        ],
        duration: 640,
    },
    // the bin rocks on its base
    trash: {
        frames: [
            { transform: 'rotate(0deg)' },
            { transform: 'rotate(-14deg)', offset: 0.2 },
            { transform: 'rotate(10deg)', offset: 0.45 },
            { transform: 'rotate(-5deg)', offset: 0.7 },
            { transform: 'rotate(0deg)' },
        ],
        duration: 560,
        origin: '50% 88%',
    },
    plus: turn(180),
    close: turn(180),
    // closing every connection: the plugs shudder apart
    'close-all': {
        frames: [
            { transform: 'translateX(0) scale(1)' },
            { transform: 'translateX(-2px) scale(1.1)', offset: 0.2 },
            { transform: 'translateX(2px) scale(1.1)', offset: 0.4 },
            { transform: 'translateX(-1.5px) scale(1.04)', offset: 0.6 },
            { transform: 'translateX(1px) scale(1)', offset: 0.8 },
            { transform: 'translateX(0) scale(1)' },
        ],
        duration: 480,
    },
    prohibit: turn(-180),
    search: {
        frames: [
            { transform: 'rotate(0deg) scale(1)' },
            { transform: 'rotate(-16deg) scale(1.18)', offset: 0.4 },
            { transform: 'rotate(0deg) scale(1)' },
        ],
        duration: 520,
    },
    'caret-left': nudge(-3, 0),
    'caret-right': nudge(3, 0),
}

let springEasing: string | undefined

// The same spring as --ease-icon, read from the stylesheet so CSS and script stay in step.
function spring () {
    if (springEasing === undefined) {
        const value = getComputedStyle(document.documentElement).getPropertyValue('--ease-icon').trim()
        springEasing = value || OUT
    }
    return springEasing
}

function reducedMotion () {
    return typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches
}

// Plays the gesture of the first icon inside `host`, if that icon has one.
export function playIconGesture (host: Element | null) {
    if (host == null || reducedMotion()) return
    const icon = host.querySelector<HTMLElement>('.icon[data-icon]')
    const gesture = icon?.dataset.icon ? gestures[icon.dataset.icon] : undefined
    if (icon == null || gesture == null || typeof icon.animate !== 'function') return

    // Decoration only: whatever happens here must never stand between a click and its action.
    try {
        icon.getAnimations?.().forEach(animation => animation.cancel())
        icon.style.transformOrigin = gesture.origin ?? '50% 50%'
        icon.animate(gesture.frames, {
            duration: gesture.duration,
            easing: gesture.easing === 'spring' ? spring() : (gesture.easing ?? OUT),
        })
    } catch {
        // an engine without linear() easing, say; the click still goes through
    }
}
