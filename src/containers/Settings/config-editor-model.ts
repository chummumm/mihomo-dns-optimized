export const MAX_CONFIG_FILE_BYTES = 4 * 1024 * 1024

export interface EditableConfigFile {
    path: string
    content: string
    revision: string
    max_size: number
    writable: boolean
}

// Textarea offsets use LF. Preserve an existing CRLF file when saving edits.
export function configDisplayText (text: string) {
    return text.includes('\r') ? text.replace(/\r\n?/g, '\n') : text
}

export function configSaveText (text: string, original: string) {
    return original.includes('\r\n') ? text.replace(/\n/g, '\r\n') : text
}

// Count UTF-8 bytes without allocating a second copy of the configuration.
// Stop as soon as the caller's limit is exceeded, including malformed surrogates.
export function configFits (text: string, limit: number) {
    if (text.length > limit) return false
    let bytes = 0
    for (let index = 0; index < text.length; index++) {
        const code = text.charCodeAt(index)
        if (code < 0x80) bytes++
        else if (code < 0x800) bytes += 2
        else if (code >= 0xd800 && code <= 0xdbff && index + 1 < text.length && text.charCodeAt(index + 1) >= 0xdc00 && text.charCodeAt(index + 1) <= 0xdfff) {
            bytes += 4
            index++
        } else bytes += 3
        if (bytes > limit) return false
    }
    return true
}

export function readConfigFile (value: unknown): EditableConfigFile | undefined {
    if (value === null || typeof value !== 'object') return undefined
    const file = value as Partial<EditableConfigFile>
    if (typeof file.path !== 'string' || file.path.length === 0 || file.path.length > 4096 ||
        typeof file.content !== 'string' || typeof file.revision !== 'string' || file.revision.length === 0 || file.revision.length > 512 ||
        typeof file.max_size !== 'number' || !Number.isSafeInteger(file.max_size) || file.max_size < 1 ||
        ('writable' in file && typeof file.writable !== 'boolean')) return undefined
    const limit = Math.min(file.max_size, MAX_CONFIG_FILE_BYTES)
    if (!configFits(file.content, limit)) return undefined
    // Older cores do not advertise this capability. Preserve their existing
    // editor behavior, while a provided field must be an actual boolean.
    return { path: file.path, content: file.content, revision: file.revision, max_size: limit, writable: file.writable ?? true }
}

// Rebuilt only when text changes. A typed array bounds indexing memory to four
// bytes per line; the visible gutter never materializes the entire number list.
export function configLineStarts (text: string) {
    let count = 1
    for (let index = 0; index < text.length; index++) if (text.charCodeAt(index) === 10) count++
    const starts = new Uint32Array(count)
    let line = 1
    for (let index = 0; index < text.length; index++) if (text.charCodeAt(index) === 10) starts[line++] = index + 1
    return starts
}

export function configCursor (starts: Uint32Array, offset: number) {
    let low = 0
    let high = starts.length
    while (low + 1 < high) {
        const middle = (low + high) >>> 1
        if (starts[middle] <= offset) low = middle
        else high = middle
    }
    return { line: low + 1, column: offset - starts[low] + 1 }
}
