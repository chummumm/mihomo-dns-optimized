export const LOG_HISTORY_LIMIT = 200

export interface Log {
    type: string
    payload: string
    time: Date
}
