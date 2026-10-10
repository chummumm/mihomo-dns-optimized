import { useSyncExternalStore } from 'react'

// UI only: how many speed tests are on the wire, so the page can show that work is under way
// and how far a round has come. The tests themselves, and what they write to the store, are untouched.
export interface TestProgress {
    running: number
    // tests started and finished since the page was last idle
    total: number
    done: number
}

let state: TestProgress = { running: 0, total: 0, done: 0 }
const listeners = new Set<() => void>()

function emit (next: TestProgress) {
    state = next
    listeners.forEach(listener => listener())
}

function subscribe (listener: () => void) {
    listeners.add(listener)
    return () => { listeners.delete(listener) }
}

export const testActivity = {
    begin () {
        const fresh = state.running === 0
        emit({
            running: state.running + 1,
            total: (fresh ? 0 : state.total) + 1,
            done: fresh ? 0 : state.done,
        })
    },
    end () {
        emit({ ...state, running: Math.max(0, state.running - 1), done: Math.min(state.total, state.done + 1) })
    },
}

export function useTestsRunning () {
    return useSyncExternalStore(subscribe, () => state.running > 0)
}

export function useTestProgress () {
    return useSyncExternalStore(subscribe, () => state)
}
