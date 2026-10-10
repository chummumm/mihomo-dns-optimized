import { type Transition } from 'motion/react'
import { useSyncExternalStore } from 'react'

// One motion vocabulary for the whole dashboard. Springs are interruptible, so
// a second click mid-flight continues from wherever the element is.
export const springs = {
    // Controls, markers and indicators: quick, settles without a visible bounce.
    snap: { type: 'spring', stiffness: 560, damping: 44, mass: 0.8 },
    // "You are here" travelling along a line: a touch of settle on arrival.
    glide: { type: 'spring', stiffness: 380, damping: 30, mass: 0.9 },
    // Panels and sheets: no overshoot past their resting edge.
    sheet: { type: 'spring', bounce: 0, duration: 0.42 },
    // Lists reflowing (sorting, items arriving): soft and short.
    reflow: { type: 'spring', stiffness: 420, damping: 40, mass: 1 },
} satisfies Record<string, Transition>

const PHONE = '(max-width: 768px)'

function subscribe (callback: () => void) {
    const query = window.matchMedia(PHONE)
    query.addEventListener('change', callback)
    return () => query.removeEventListener('change', callback)
}

// Phones get sheets from the bottom edge; wider screens get panels that settle in place.
export function useIsPhone () {
    return useSyncExternalStore(subscribe, () => window.matchMedia(PHONE).matches, () => false)
}
