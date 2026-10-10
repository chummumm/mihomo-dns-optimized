import { useSyncExternalStore } from 'react'

// UI only: how many speed tests are on the wire, so the page can show that work is under way.
// The tests themselves, and what they write to the store, are untouched.
let running = 0
const listeners = new Set<() => void>()

function emit () {
    listeners.forEach(listener => listener())
}

function subscribe (listener: () => void) {
    listeners.add(listener)
    return () => { listeners.delete(listener) }
}

export const testActivity = {
    begin () {
        running += 1
        emit()
    },
    end () {
        running = Math.max(0, running - 1)
        emit()
    },
}

export function useTestsRunning () {
    return useSyncExternalStore(subscribe, () => running > 0)
}
