import { useRef, type KeyboardEvent } from 'react'
import { deviceLabel, type BacnetObject, type Device } from './api'
import { deviceRowKey, objectRowKey, parentOf, titleCase, type Row, type Selection } from './treeModel'

interface TreeProps {
  rows: Row[]
  selection: Selection | null
  // How many devices each router has behind it, by the router's address.
  behind: ReadonlyMap<string, number>
  onSelect: (s: Selection) => void
  onToggle: (key: string, open: boolean) => void
}

function selectedKey(s: Selection | null): string | null {
  if (!s) return null
  return s.kind === 'device' ? deviceRowKey(s.device) : objectRowKey(s.device, s)
}

const canOpen = (row: Row): row is Row & { open: boolean } => 'open' in row

export function Tree({ rows, selection, behind, onSelect, onToggle }: TreeProps) {
  const ref = useRef<HTMLDivElement>(null)
  const current = selectedKey(selection)
  // The row that Tab lands on: the selected one if it is showing.
  const tabStop = rows.some((r) => r.key === current) ? current : rows.find((r) => r.kind !== 'note')?.key

  const focusRow = (index: number) => {
    const row = rows[index]
    if (!row) return
    ref.current?.querySelector<HTMLElement>(`[data-key="${CSS.escape(row.key)}"]`)?.focus()
  }

  const activate = (row: Row) => {
    if (row.kind === 'device') {
      // Picking a device opens it, which is what makes the server read its
      // objects.
      onSelect({ kind: 'device', device: row.device.id })
      if (!row.open) onToggle(row.key, true)
    } else if (row.kind === 'object') {
      onSelect({ kind: 'object', device: row.device, type: row.object.type, instance: row.object.instance })
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
    <div className="tree" role="tree" aria-label="Devices and objects" ref={ref} onKeyDown={onKeyDown}>
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
        const selected = row.key === current
        return (
          <div
            key={row.key}
            data-key={row.key}
            className={`row ${row.kind}${selected ? ' selected' : ''}`}
            role="treeitem"
            aria-level={row.depth + 1}
            aria-selected={selected}
            aria-expanded={canOpen(row) ? row.open : undefined}
            tabIndex={row.key === tabStop ? 0 : -1}
            onClick={() => activate(row)}
          >
            <Guides depth={row.depth} />
            {canOpen(row) ? (
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
            {row.kind === 'device' && <DeviceRow device={row.device} behind={behind.get(row.device.address) ?? 0} />}
            {row.kind === 'router' && (
              <>
                <span className="label strong">Router</span>
                <span className="sub mono">{row.address}</span>
              </>
            )}
            {row.kind === 'network' && (
              <>
                <span className="label strong">Network {row.network}</span>
                <span className="sub">{plural(row.count, 'device')}</span>
              </>
            )}
            {row.kind === 'group' && (
              <>
                <span className="label">{titleCase(row.typeName)}</span>
                <span className="sub">{row.count}</span>
              </>
            )}
            {row.kind === 'object' && <ObjectRow object={row.object} />}
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

function DeviceRow({ device, behind }: { device: Device; behind: number }) {
  const state = device.objects
  return (
    <>
      <svg className="glyph" viewBox="0 0 16 16" aria-hidden>
        <rect x="2" y="3" width="12" height="10" rx="1.5" fill="none" stroke="currentColor" strokeWidth="1.3" />
        <path d="M5 6.5h3M5 9.5h6" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" />
      </svg>
      <span className="label strong">{deviceLabel(device)}</span>
      <span className="sub mono">#{device.id}</span>
      <span className="spacer" />
      {behind > 0 && device.network === 0 && <span className="pill">routes {plural(behind, 'device')}</span>}
      {state.state === 'loading' && (
        <span className="sub">{state.total > 0 ? `${state.done}/${state.total}` : ''}</span>
      )}
      {state.state === 'loading' && <span className="spinner" aria-label="Reading objects" />}
      {state.state === 'ready' && <span className="sub">{plural(state.total, 'object')}</span>}
      {state.state === 'error' && <span className="flag alarm">error</span>}
      <span className="sub mono address">{device.network === 0 ? device.address : `MAC ${device.mac}`}</span>
    </>
  )
}

const flagClass: Record<string, string> = {
  'in alarm': 'alarm',
  fault: 'fault',
  overridden: 'override',
  'out of service': 'oos',
}

export function family(o: { typeName: string }): string {
  if (o.typeName.startsWith('analog') || o.typeName.endsWith('analog-value')) return 'analog'
  if (o.typeName.startsWith('binary')) return 'binary'
  if (o.typeName.startsWith('multi-state')) return 'multistate'
  return 'other'
}

function ObjectRow({ object: o }: { object: BacnetObject }) {
  return (
    <>
      <span className={`chip ${family(o)}`}>{o.abbr}</span>
      {o.name ? (
        <span className="label" title={o.description || undefined}>
          {o.name}
        </span>
      ) : (
        <span className={`label ${o.loaded ? 'unnamed' : 'pending'}`} title={o.error}>
          {o.typeName}
        </span>
      )}
      <span className="sub mono">{o.instance}</span>
      <span className="spacer" />
      {o.flags?.map((f) => (
        <span key={f} className={`flag ${flagClass[f] ?? ''}`}>
          {f}
        </span>
      ))}
      {o.value !== undefined && o.value !== '' && (
        <span className="value mono">
          {o.value}
          {o.units && <span className="units">{o.units}</span>}
        </span>
      )}
    </>
  )
}
