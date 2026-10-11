import axios from 'axios'
import classnames from 'classnames'
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type MutableRefObject } from 'react'

import { Button, Icon, Modal } from '@components'
import { type Client } from '@lib/request'
import { useClient, useGeneral, useI18n } from '@stores'

import { MAX_CONFIG_FILE_BYTES, configCursor, configDisplayText, configFits, configLineStarts, configSaveText, readConfigFile, type EditableConfigFile } from './config-editor-model'
import './config-editor.css'

interface ConfigEditorProps {
    show: boolean
    onClose: () => void
}

type EditorError = 'unsupported' | 'unauthorized' | 'fileAccess' | 'readOnly' | 'tooLarge' | 'invalid' | 'conflict' | 'format' | 'path' | 'busy' | 'network' | 'invalidResponse' | 'reloadInvalid' | 'reload'

function classifyError (reason: unknown): EditorError {
    const status = axios.isAxiosError(reason) ? reason.response?.status : undefined
    if (status === 404 || status === 405) return 'unsupported'
    if (status === 403 && axios.isAxiosError(reason) && reason.response?.data?.code === 'config_file_read_only') return 'readOnly'
    if (status === 403 && axios.isAxiosError(reason) && reason.response?.data?.code === 'config_file_access_denied') return 'fileAccess'
    if (status === 401 || status === 403) return 'unauthorized'
    if (status === 409) return 'conflict'
    if (status === 413) return 'tooLarge'
    if (status === 415) return 'format'
    if (status === 422) return 'path'
    if (status === 503) return 'busy'
    if (status === 400) return 'invalid'
    // Server errors may contain configuration fragments or credentials. Keep
    // them out of the UI, notifications and persistent application stores.
    return 'network'
}

export function ConfigEditor ({ show, onClose }: ConfigEditorProps) {
    const client = useClient()
    const { translation } = useI18n()
    const { t } = translation('Settings')
    const closeHandler = useRef<(() => void) | null>(null)
    const controller = useRef({ client, id: 0 })
    if (controller.current.client !== client) controller.current = { client, id: controller.current.id + 1 }

    return (
        <Modal show={show} title={t('editor.title')} size="big" className="config-editor-modal" bodyClassName="config-editor-body" footer={false} onClose={() => {
            if (closeHandler.current !== null) closeHandler.current()
            else onClose()
        }}>
            {show && <EditorSession key={controller.current.id} client={client} onClose={onClose} closeHandler={closeHandler} />}
        </Modal>
    )
}

// Only mounted while this controller's editor is open. Closing or changing
// controller disposes the draft, original text, revision and in-flight requests.
function EditorSession ({ client, onClose, closeHandler }: {
    client: Client
    onClose: () => void
    closeHandler: MutableRefObject<(() => void) | null>
}) {
    const { translation } = useI18n()
    const { t } = translation('Settings')
    const { update: fetchGeneral } = useGeneral()
    const [file, setFile] = useState<EditableConfigFile | null>(null)
    const [text, setText] = useState('')
    const [cursor, setCursor] = useState({ line: 1, column: 1 })
    const [loading, setLoading] = useState(true)
    const [saving, setSaving] = useState(false)
    const [reloading, setReloading] = useState(false)
    const [reloadPending, setReloadPending] = useState(false)
    const [saved, setSaved] = useState(false)
    const [problem, setProblem] = useState<EditorError | null>(null)
    const [conflict, setConflict] = useState(false)
    const [confirmation, setConfirmation] = useState<'close' | 'read' | null>(null)
    const [viewport, setViewport] = useState({ top: 0, height: 360, lineHeight: 20.4, paddingTop: 12 })
    const area = useRef<HTMLTextAreaElement>(null)
    const lineMeasure = useRef<HTMLPreElement>(null)
    const pendingCaret = useRef<number>()
    const resetPosition = useRef(false)
    const requestRef = useRef<AbortController | null>(null)
    const active = useRef(true)
    const root = useRef<HTMLDivElement>(null)
    const closeButton = useRef<HTMLButtonElement>(null)
    const original = useMemo(() => file !== null ? configDisplayText(file.content) : '', [file])
    const dirty = useMemo(() => file !== null && text !== original, [file, original, text])
    const lines = useMemo(() => configLineStarts(text), [text])
    const limit = file?.max_size ?? MAX_CONFIG_FILE_BYTES
    const readOnly = file?.writable === false

    const load = useCallback(async () => {
        if (requestRef.current !== null) return
        const request = new AbortController()
        requestRef.current = request
        setLoading(true)
        setFile(null)
        setText('')
        setProblem(null)
        setConflict(false)
        setSaved(false)
        setReloadPending(false)
        setConfirmation(null)
        setCursor({ line: 1, column: 1 })
        pendingCaret.current = undefined
        resetPosition.current = false
        try {
            const response = await client.getConfigFile(request.signal)
            if (!active.current || request.signal.aborted || requestRef.current !== request) return
            const result = readConfigFile(response.data)
            if (result === undefined) { setProblem('invalidResponse'); return }
            resetPosition.current = true
            setFile(result)
            setText(configDisplayText(result.content))
        } catch (reason) {
            if (active.current && !request.signal.aborted && requestRef.current === request) setProblem(classifyError(reason))
        } finally {
            if (requestRef.current === request) {
                requestRef.current = null
                if (active.current) setLoading(false)
            }
        }
    }, [client])

    useEffect(() => {
        active.current = true
        void load()
        return () => {
            active.current = false
            requestRef.current?.abort()
            requestRef.current = null
            pendingCaret.current = undefined
            resetPosition.current = false
        }
    }, [load])

    const track = useCallback(() => {
        if (area.current === null) return
        const next = configCursor(lines, area.current.selectionStart)
        setCursor(previous => previous.line === next.line && previous.column === next.column ? previous : next)
    }, [lines])

    const measure = useCallback(() => {
        const element = area.current
        if (element === null) return
        const style = window.getComputedStyle(element)
        // CSSOM's line-height may be unrounded (20.4px) while layout uses a
        // 20.390625px line box. Measure eight actual lines independently of the
        // virtual gutter; otherwise the accumulated error hides lines at EOF.
        const usedLineHeight = lineMeasure.current === null ? 0 : parseFloat(window.getComputedStyle(lineMeasure.current).height) / 8
        const next = {
            top: element.scrollTop,
            height: element.clientHeight,
            lineHeight: usedLineHeight || parseFloat(style.lineHeight) || 20.4,
            paddingTop: parseFloat(style.paddingTop) || 12,
        }
        setViewport(previous => Object.keys(next).every(key => previous[key as keyof typeof next] === next[key as keyof typeof next]) ? previous : next)
    }, [])

    useLayoutEffect(() => {
        const element = area.current
        if (element === null) return
        if (resetPosition.current && !loading) {
            // Assigning a textarea value can leave its selection at EOF. Reset
            // only after an actual file read, before focusing or painting; a
            // save response must not move the user's selection or viewport.
            resetPosition.current = false
            element.setSelectionRange(0, 0)
            element.scrollTop = element.scrollLeft = 0
            element.focus({ preventScroll: true })
            element.scrollTop = element.scrollLeft = 0
            measure()
        } else if (pendingCaret.current !== undefined) {
            element.selectionStart = element.selectionEnd = pendingCaret.current
            pendingCaret.current = undefined
        }
        track()
    }, [loading, text, track, measure])

    useLayoutEffect(() => {
        const element = area.current
        if (element === null) return
        measure()
        const observer = new ResizeObserver(measure)
        observer.observe(element)
        if (lineMeasure.current !== null) observer.observe(lineMeasure.current)
        return () => observer.disconnect()
    }, [measure])

    useEffect(() => {
        if (!dirty && !saving) return
        function beforeUnload (event: BeforeUnloadEvent) {
            event.preventDefault()
            event.returnValue = ''
        }
        window.addEventListener('beforeunload', beforeUnload)
        return () => window.removeEventListener('beforeunload', beforeUnload)
    }, [dirty, saving])

    useLayoutEffect(() => {
        closeHandler.current = () => {
            if (requestRef.current !== null && saving) return
            if (confirmation !== null) { setConfirmation(null); area.current?.focus(); return }
            if (dirty) setConfirmation('close')
            else onClose()
        }
        return () => { closeHandler.current = null }
    }, [closeHandler, confirmation, dirty, onClose, saving])

    useEffect(() => {
        if (confirmation !== null) closeButton.current?.focus()
    }, [confirmation])

    async function save () {
        if (file === null || readOnly || (!dirty && !reloadPending) || conflict || requestRef.current !== null || confirmation !== null) return
        const content = configSaveText(text, file.content)
        if (!configFits(content, limit)) { setProblem('tooLarge'); return }
        const request = new AbortController()
        requestRef.current = request
        setSaving(true)
        setReloading(false)
        setProblem(null)
        setSaved(false)
        let fileSaved = false
        try {
            // A reload retry still checks the saved revision. The core's
            // unchanged-content path does not rewrite the file, and rejects
            // a conflicting external edit before we can reload it.
            const response = await client.saveConfigFile(content, file.revision, request.signal)
            if (!active.current || request.signal.aborted || requestRef.current !== request) return
            const result = readConfigFile(response.data)
            if (result === undefined) { setProblem('invalidResponse'); return }
            setFile(result)
            setText(configDisplayText(result.content))
            fileSaved = true
            setReloadPending(true)
            setReloading(true)
            // Include listener settings such as port / mixed-port / allow-lan.
            // This reads the current file, never an arbitrary path or payload.
            await client.reloadConfig(request.signal, true)
            if (!active.current || request.signal.aborted || requestRef.current !== request) return
            setReloadPending(false)
            setSaved(true)
            // Reload is complete. A failed settings refresh must not turn it
            // into a reported reload failure or keep the editor locked.
            void fetchGeneral().catch(() => undefined)
        } catch (reason) {
            if (!active.current || request.signal.aborted || requestRef.current !== request) return
            const error = fileSaved
                ? axios.isAxiosError(reason) && reason.response?.status === 400 ? 'reloadInvalid' : 'reload'
                : classifyError(reason)
            setProblem(error)
            if (error === 'conflict') setConflict(true)
            if (error === 'readOnly' || error === 'fileAccess') setFile(current => current === null ? current : { ...current, writable: false })
        } finally {
            if (requestRef.current === request) {
                requestRef.current = null
                if (active.current) {
                    setSaving(false)
                    setReloading(false)
                }
            }
        }
    }

    function edit (value: string) {
        if (file === null || loading || readOnly || saving || confirmation !== null) return false
        if (!configFits(value, limit)) { setProblem('tooLarge'); return false }
        setText(value)
        setSaved(false)
        if (!conflict) setProblem(null)
        return true
    }

    function keydown (event: KeyboardEvent<HTMLTextAreaElement>) {
        if (event.key === 'Tab' && !event.shiftKey && !event.altKey && !event.ctrlKey && !event.metaKey && !readOnly && !saving && confirmation === null) {
            event.preventDefault()
            const element = event.currentTarget
            const start = element.selectionStart
            if (edit(`${text.slice(0, start)}  ${text.slice(element.selectionEnd)}`)) pendingCaret.current = start + 2
        }
        if (event.key.toLowerCase() === 's' && (event.ctrlKey || event.metaKey)) {
            event.preventDefault()
            void save()
        }
    }

    function dialogKeydown (event: KeyboardEvent<HTMLDivElement>) {
        if (!event.defaultPrevented && event.key.toLowerCase() === 's' && (event.ctrlKey || event.metaKey)) {
            event.preventDefault()
            void save()
            return
        }
        if (event.key !== 'Tab' || event.defaultPrevented) return
        const controls = root.current?.querySelectorAll<HTMLButtonElement | HTMLTextAreaElement>('button:not(:disabled), textarea:not(:disabled)')
        if (controls === undefined || controls.length === 0) return
        const first = controls[0]
        const last = controls[controls.length - 1]
        if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus() } else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus() }
    }

    function reread () {
        if (requestRef.current !== null) return
        if (dirty) setConfirmation('read')
        else void load()
    }

    const firstLine = Math.max(0, Math.min(lines.length - 1, Math.floor((viewport.top - viewport.paddingTop) / viewport.lineHeight) - 2))
    const lastLine = Math.min(lines.length, firstLine + Math.ceil(viewport.height / viewport.lineHeight) + 5)
    const numbers = useMemo(() => Array.from({ length: lastLine - firstLine }, (_, index) => firstLine + index + 1).join('\n'), [firstLine, lastLine])

    return (
        <div ref={root} className="config-editor-content" role="dialog" aria-modal="true" aria-label={t('editor.title')} onKeyDown={dialogKeydown}>
            <div className="config-editor-bar">
                <span className="config-editor-file" title={file?.path}>
                    <Icon type="file" size={14} />
                    <span>{file?.path ?? t('editor.noFile')}</span>
                </span>
                <span className={classnames('config-editor-state', { 'is-connected': file !== null && !conflict && !readOnly })}>
                    <i aria-hidden="true" />
                    {loading ? t('editor.loading') : saving ? t(reloading ? 'editor.reloading' : 'editor.saving') : readOnly ? t('editor.readOnly') : dirty ? t('editor.unsaved') : reloadPending ? t('editor.pendingReload') : file !== null ? 'YAML' : t('editor.unavailable')}
                </span>
            </div>
            <div className={classnames('config-editor-notice', { 'is-error': problem !== null, 'is-saved': saved })} role={problem !== null ? 'alert' : 'status'}>
                {problem !== null ? t(`editor.errors.${problem}`) : readOnly ? t('editor.readOnlyNote') : saved ? t('editor.saved') : t('editor.note')}
            </div>
            <div className="config-editor-frame" aria-busy={loading || saving}>
                <pre ref={lineMeasure} className="config-editor-line-measure" aria-hidden="true">{'0\n0\n0\n0\n0\n0\n0\n0'}</pre>
                <div className="config-editor-gutter" aria-hidden="true" style={{ minWidth: `calc(${Math.max(2, String(lines.length).length)}ch + 22px)` }}>
                    <pre style={{ transform: `translateY(${viewport.paddingTop + firstLine * viewport.lineHeight - viewport.top}px)` }}>{numbers}</pre>
                </div>
                <textarea
                    ref={area}
                    className="config-editor-text"
                    value={text}
                    aria-label={t('editor.title')}
                    readOnly={readOnly || saving || confirmation !== null}
                    disabled={file === null || loading}
                    maxLength={limit}
                    spellCheck={false}
                    autoCapitalize="off"
                    autoComplete="off"
                    autoCorrect="off"
                    data-gramm="false"
                    data-gramm_editor="false"
                    data-enable-grammarly="false"
                    wrap="off"
                    onChange={event => { edit(event.target.value) }}
                    onKeyDown={keydown}
                    onKeyUp={track}
                    onClick={track}
                    onSelect={track}
                    onScroll={measure} />
                {file === null && <div className="config-editor-empty" aria-hidden="true">
                    <Icon type={loading ? 'update' : 'edit'} size={22} />
                    <b>{loading ? t('editor.loading') : t('editor.emptyTitle')}</b>
                    <span>{loading ? t('editor.loadingHint') : t('editor.emptyHint')}</span>
                </div>}
            </div>
            <div className="config-editor-status">
                <span className="config-editor-note">{t('editor.validationNote')}</span>
                <span className="reading">{t('editor.line')} {cursor.line} · {t('editor.column')} {cursor.column}</span>
                <span className="reading">YAML · UTF-8</span>
            </div>
            {confirmation !== null
                ? <div className="config-editor-confirm" role="alert">
                    <span>{t(confirmation === 'close' ? 'editor.discardClose' : 'editor.discardRead')}</span>
                    <div className="config-editor-footer">
                        <button ref={closeButton} type="button" className="button button-normal button-md" onClick={() => setConfirmation(null)}>{t('editor.keepEditing')}</button>
                        <Button type="danger" onClick={() => {
                            if (confirmation === 'close') onClose()
                            else void load()
                        }}>{t(confirmation === 'close' ? 'editor.discard' : 'editor.discardAndRead')}</Button>
                    </div>
                </div>
                : <div className="config-editor-footer">
                    <Button icon="update" disabled={loading || saving} onClick={reread}>{t(file === null ? 'editor.retry' : 'editor.reread')}</Button>
                    <span className="config-editor-footer-spacer" />
                    <Button disabled={saving} onClick={() => closeHandler.current?.()}>{t('editor.close')}</Button>
                    <Button type="primary" icon={reloading ? 'update' : 'save'} loading={saving} disabled={file === null || readOnly || loading || saving || (!dirty && !reloadPending) || conflict} onClick={() => { void save() }}>{t(saving ? reloading ? 'editor.reloading' : 'editor.saving' : reloadPending && !dirty ? 'editor.retryReload' : 'editor.save')}</Button>
                </div>}
        </div>
    )
}
