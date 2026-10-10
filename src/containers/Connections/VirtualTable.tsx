import { flexRender, type Row } from '@tanstack/react-table'
import classnames from 'classnames'
import { Component, createContext, createRef, forwardRef, useContext, type CSSProperties, type HTMLAttributes, type ReactNode } from 'react'
import { FixedSizeList, type ListChildComponentProps } from 'react-window'

import { type FormatConnection } from './store'

const ROW_HEIGHT = 36
const HEADER_HEIGHT = 30

interface TableProps {
    rows: Array<Row<FormatConnection>>
    headers: ReactNode
    height: number
    width: number
    totalWidth: number
    centeredColumns: Set<string>
    selectedID?: string
    onSelect: (id: string) => void
}

interface TableContextValue {
    headers: ReactNode
    totalWidth: number
    count: number
    horizontalScrolled: boolean
    onHorizontalScroll: (left: number) => void
}

const TableContext = createContext<TableContextValue>({
    headers: null,
    totalWidth: 0,
    count: 0,
    horizontalScrolled: false,
    onHorizontalScroll: () => {},
})

const TableOuter = forwardRef<HTMLDivElement, HTMLAttributes<HTMLDivElement>>((props, ref) => {
    const context = useContext(TableContext)
    return (
        <div
            {...props}
            ref={ref}
            className={classnames('connections-scroll', { 'horizontally-scrolled': context.horizontalScrolled })}
            onScroll={event => {
                props.onScroll?.(event)
                context.onHorizontalScroll(event.currentTarget.scrollLeft)
            }} />
    )
})

const TableInner = forwardRef<HTMLDivElement, HTMLAttributes<HTMLDivElement>>(({ children, style, ...props }, ref) => {
    const context = useContext(TableContext)
    const innerStyle: CSSProperties = {
        ...style,
        height: Number(style?.height ?? 0) + HEADER_HEIGHT,
        width: context.totalWidth,
    }
    return (
        <div {...props} ref={ref} style={innerStyle} role="table" aria-rowcount={context.count + 1}>
            <div className="connections-header" role="row" aria-rowindex={1}>{context.headers}</div>
            {children}
        </div>
    )
})

function ConnectionRow ({ index, style, data }: ListChildComponentProps<TableProps>) {
    const row = data.rows[index]
    return (
        <div
            role="row"
            aria-rowindex={index + 2}
            className={classnames('connections-row', { selected: data.selectedID === row.original.id, completed: row.original.completed })}
            data-connection-id={row.original.id}
            style={{ ...style, top: Number(style.top ?? 0) + HEADER_HEIGHT, width: data.totalWidth }}
            onClick={() => data.onSelect(row.original.id)}>
            {row.getAllCells().map(cell => (
                <div
                    role="cell"
                    className={classnames('connections-block', {
                        'text-center': data.centeredColumns.has(cell.column.id),
                        completed: row.original.completed,
                        fixed: cell.column.id === 'host',
                    })}
                    style={{ width: cell.column.getSize() }}
                    key={cell.column.id}>
                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
                </div>
            ))}
        </div>
    )
}

function itemKey (index: number, data: TableProps) {
    return data.rows[index].original.id
}

interface ScrollAnchor {
    index: number
    top: number
    left: number
    atTop: boolean
    atBottom: boolean
    candidates: Array<{ id: string, offset: number }>
}

// React's snapshot lifecycle reads the old DOM immediately before a commit.
// Native anchoring is disabled so it cannot apply a second, conflicting shift.
export class VirtualConnectionTable extends Component<TableProps, { horizontalScrolled: boolean }, ScrollAnchor | null> {
    state = { horizontalScrolled: false }
    private readonly outer = createRef<HTMLDivElement>()
    private readonly list = createRef<FixedSizeList<TableProps>>()
    private indexedRows: TableProps['rows'] | undefined
    private rowIndices: Map<string, number> | undefined

    getSnapshotBeforeUpdate (previous: TableProps): ScrollAnchor | null {
        const outer = this.outer.current
        if (outer === null || (previous.rows === this.props.rows && previous.height === this.props.height && previous.width === this.props.width && previous.totalWidth === this.props.totalWidth)) return null
        const first = Math.max(0, Math.floor(outer.scrollTop / ROW_HEIGHT))
        const visible = Math.ceil(outer.clientHeight / ROW_HEIGHT) + 1
        const maximum = Math.max(0, outer.scrollHeight - outer.clientHeight)
        return {
            index: first,
            top: outer.scrollTop,
            left: outer.scrollLeft,
            atTop: outer.scrollTop <= 1,
            atBottom: maximum > 0 && maximum - outer.scrollTop <= 2,
            candidates: previous.rows.slice(first, first + visible).map((row, index) => ({
                id: row.original.id,
                offset: outer.scrollTop - (first + index) * ROW_HEIGHT,
            })),
        }
    }

    componentDidUpdate (_previous: TableProps, _state: { horizontalScrolled: boolean }, anchor: ScrollAnchor | null) {
        if (this.indexedRows !== this.props.rows) {
            this.indexedRows = undefined
            this.rowIndices = undefined
        }
        const outer = this.outer.current
        if (anchor === null || outer === null) return
        const maximum = Math.max(0, outer.scrollHeight - outer.clientHeight)
        let top = Math.min(anchor.top, maximum)
        if (anchor.atTop) {
            top = 0
        } else if (anchor.atBottom) {
            top = maximum
        } else if (this.props.rows[anchor.index]?.original.id !== anchor.candidates[0]?.id) {
            if (this.rowIndices === undefined) {
                this.rowIndices = new Map()
                this.props.rows.forEach((row, index) => this.rowIndices!.set(row.original.id, index))
                this.indexedRows = this.props.rows
            }
            for (const candidate of anchor.candidates) {
                const index = this.rowIndices.get(candidate.id)
                if (index !== undefined) {
                    top = Math.max(0, Math.min(index * ROW_HEIGHT + candidate.offset, maximum))
                    break
                }
            }
        }
        this.list.current?.scrollTo(top)
        outer.scrollTop = top
        outer.scrollLeft = anchor.left
    }

    private readonly onHorizontalScroll = (left: number) => {
        const horizontalScrolled = left > 0
        if (this.state.horizontalScrolled !== horizontalScrolled) this.setState({ horizontalScrolled })
    }

    render () {
        const totalWidth = Math.max(this.props.totalWidth, this.props.width)
        const data = { ...this.props, totalWidth }
        return (
            <TableContext.Provider value={{
                headers: this.props.headers,
                totalWidth,
                count: this.props.rows.length,
                horizontalScrolled: this.state.horizontalScrolled,
                onHorizontalScroll: this.onHorizontalScroll,
            }}>
                <FixedSizeList<TableProps>
                    ref={this.list}
                    outerRef={this.outer}
                    outerElementType={TableOuter}
                    innerElementType={TableInner}
                    width={this.props.width}
                    height={this.props.height}
                    itemCount={this.props.rows.length}
                    itemSize={ROW_HEIGHT}
                    itemData={data}
                    itemKey={itemKey}
                    overscanCount={6}>
                    {ConnectionRow}
                </FixedSizeList>
            </TableContext.Provider>
        )
    }
}
