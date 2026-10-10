import { flexRender, getCoreRowModel, useReactTable, type ColumnDef } from '@tanstack/react-table'

interface TableProps<T> {
    rows: T[]
    columns: Array<ColumnDef<T>>
    rowID: (row: T) => string
    empty: string
    busy?: boolean
}

export function DNSTable<T> ({ rows, columns, rowID, empty, busy }: TableProps<T>) {
    const table = useReactTable({
        data: rows,
        columns,
        getRowId: rowID,
        getCoreRowModel: getCoreRowModel(),
        manualPagination: true,
    })

    return <div className="dns-table-scroll" aria-busy={busy}>
        <table className="dns-table">
            <thead>
                {table.getHeaderGroups().map(group => <tr key={group.id}>
                    {group.headers.map(header => <th key={header.id} scope="col">
                        {flexRender(header.column.columnDef.header, header.getContext())}
                    </th>)}
                </tr>)}
            </thead>
            <tbody>
                {table.getRowModel().rows.map(row => <tr key={row.id}>
                    {row.getVisibleCells().map(cell => <td key={cell.id}>
                        {flexRender(cell.column.columnDef.cell, cell.getContext())}
                    </td>)}
                </tr>)}
                {rows.length === 0 && <tr><td colSpan={columns.length} className="dns-empty">{empty}</td></tr>}
            </tbody>
        </table>
    </div>
}
