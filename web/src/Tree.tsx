import { useRef, type KeyboardEvent } from 'react'
import type { Node, NodeID } from './api'
import { parentOf, type Row } from './treeModel'

interface TreeProps {
  rows: Row[]
  selection: NodeID | null
  onSelect: (id: NodeID) => void
  onToggle: (key: string, open: boolean) => void
}

const canOpen = (row: Row): row is Row & { open: boolean } => 'open' in row

export function Tree({ rows, selection, onSelect, onToggle }: TreeProps) {
  const ref = useRef<HTMLDivElement>(null)
  // The row that Tab lands on: the selected one if it is showing.
  const tabStop = rows.some((r) => r.key === selection) ? selection : rows.find((r) => r.kind !== 'note')?.key

  const focusRow = (index: number) => {
    const row = rows[index]
    if (!row) return
    ref.current?.querySelector<HTMLElement>(`[data-key="${CSS.escape(row.key)}"]`)?.focus()
  }

  const activate = (row: Row) => {
    if (row.kind === 'node') {
      onSelect(row.node.id)
      // Picking a node with children opens it, which is what makes the
      // server read them.
      if (row.node.children.state !== 'none' && !row.open) onToggle(row.key, true)
    } else if (canOpen(row)) {
      onToggle(row.key, !row.open)
    }
  }

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const key = (e.target as HTMLElement).dataset.key
    const index = rows.findIndex((r) => r.key === key)
    if (index < 0) return
    const row = rows[index]
    const step = (from: number, by: number) => {
      for (let i = from + by; i >= 0 && i < rows.length; i += by) {
        if (rows[i].kind !== 'note') return i
      }
      return from
    }
    switch (e.key) {
      case 'ArrowDown':
        focusRow(step(index, 1))
        break
      case 'ArrowUp':
        focusRow(step(index, -1))
        break
      case 'Home':
        focusRow(step(-1, 1))
        break
      case 'End':
        focusRow(step(rows.length, -1))
        break
      case 'ArrowRight':
        if (canOpen(row) && !row.open) onToggle(row.key, true)
        else if (canOpen(row)) focusRow(step(index, 1))
        break
      case 'ArrowLeft':
        if (canOpen(row) && row.open) onToggle(row.key, false)
        else focusRow(parentOf(rows, index))
        break
      case 'Enter':
      case ' ':
        activate(row)
        break
      default:
        return
    }
    e.preventDefault()
  }

  return (
    <div className="tree" role="tree" aria-label="Devices and points" ref={ref} onKeyDown={onKeyDown}>
      {rows.map((row) => {
        if (row.kind === 'note') {
          return (
            <div key={row.key} className={`row note ${row.tone}`} role="none">
              <Guides depth={row.depth} />
              <span className="twisty" />
              {row.tone === 'busy' && <span className="spinner" aria-hidden />}
              <span className="label">{row.text}</span>
            </div>
          )
        }
        const selected = row.key === selection
        const openable = row.kind === 'group' || row.node.children.state !== 'none'
        const rowClass = row.kind === 'group' ? 'group' : row.node.kind === 'point' ? 'object' : row.node.kind
        return (
          <div
            key={row.key}
            data-key={row.key}
            className={`row ${rowClass}${selected ? ' selected' : ''}`}
            role="treeitem"
            aria-level={row.depth + 1}
            aria-selected={selected}
            aria-expanded={openable ? row.open : undefined}
            tabIndex={row.key === tabStop ? 0 : -1}
            onClick={() => activate(row)}
          >
            <Guides depth={row.depth} />
            {openable ? (
              <span
                className={`twisty${row.open ? ' open' : ''}`}
                onClick={(e) => {
                  e.stopPropagation()
                  onToggle(row.key, !row.open)
                }}
              >
                <svg viewBox="0 0 16 16" aria-hidden>
                  <path d="M6 4l4 4-4 4" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
                </svg>
              </span>
            ) : (
              <span className="twisty" />
            )}
            {row.kind === 'group' ? (
              <>
                <span className="label">{row.name}</span>
                <span className="sub">{row.count}</span>
              </>
            ) : (
              <NodeRow node={row.node} count={row.count} />
            )}
          </div>
        )
      })}
    </div>
  )
}

function Guides({ depth }: { depth: number }) {
  return (
    <>
      {Array.from({ length: depth }, (_, i) => (
        <span key={i} className="guide" />
      ))}
    </>
  )
}

export const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`

const flagClass: Record<string, string> = {
  'in alarm': 'alarm',
  fault: 'fault',
  overridden: 'override',
  'out of service': 'oos',
}

function NodeRow({ node: n, count }: { node: Node; count: number }) {
  const state = n.children
  const strong = n.kind !== 'point'
  return (
    <>
      {n.kind === 'device' && (
        <svg className="glyph" viewBox="0 0 16 16" aria-hidden>
          <rect x="2" y="3" width="12" height="10" rx="1.5" fill="none" stroke="currentColor" strokeWidth="1.3" />
          <path d="M5 6.5h3M5 9.5h6" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" />
        </svg>
      )}
      {n.tag && <span className={`chip ${n.family ?? 'other'}`}>{n.tag}</span>}
      <span
        className={`label${strong ? ' strong' : ''}${n.pending ? ' pending' : ''}${n.error ? ' unnamed' : ''}`}
        title={n.error || n.facts?.find((f) => f.label === 'Description')?.value || undefined}
      >
        {n.label}
      </span>
      {n.sublabel && <span className="sub mono">{n.sublabel}</span>}
      <span className="spacer" />
      {n.kind === 'group' && <span className="sub">{plural(count, 'item')}</span>}
      {state.state === 'loading' && n.kind !== 'group' && (
        <span className="sub">{state.total > 0 ? `${state.done}/${state.total}` : ''}</span>
      )}
      {state.state === 'loading' && <span className="spinner" aria-label="Reading" />}
      {state.state === 'ready' && n.kind !== 'group' && <span className="sub">{plural(state.total, 'item')}</span>}
      {state.state === 'error' && <span className="flag alarm">error</span>}
      {n.reading?.flags?.map((f) => (
        <span key={f} className={`flag ${flagClass[f] ?? ''}`}>
          {f}
        </span>
      ))}
      {n.reading && n.reading.text !== '' && (
        <span className="value mono">
          {n.reading.text}
          {n.reading.units && <span className="units">{n.reading.units}</span>}
        </span>
      )}
      {n.meta && <span className="sub mono address">{n.meta}</span>}
    </>
  )
}
