import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type DetailRow, type Node } from './api'
import { takeChildren } from './live'
import { plural } from './Tree'

export function EmptyDetail({ nodes, discovering }: { nodes: number; discovering: boolean }) {
  return (
    <div className="empty">
      <h2>{nodes === 0 ? 'Listening' : 'Pick something'}</h2>
      <p>
        {nodes === 0
          ? discovering
            ? 'Devices appear on the left as they answer; Scan asks again.'
            : 'Nothing has been found yet.'
          : 'Picking a device reads what is under it. Picking a point reads everything about it.'}
      </p>
    </div>
  )
}

interface NodeDetailProps {
  node: Node
  // What hangs under the node, for the counts.
  under: Node[]
}

export function NodeDetail({ node, under }: NodeDetailProps) {
  const qc = useQueryClient()
  const reload = useMutation({
    mutationFn: () => api.refresh(node.id),
    onSuccess: (list) => qc.setQueryData(['rev', node.id], takeChildren(qc, node.id, list)),
  })
  const detail = useQuery({
    queryKey: ['detail', node.id],
    queryFn: () => api.detail(node.id),
    // A node picked again is read again: the values are live.
    staleTime: 0,
    gcTime: 0,
    enabled: node.kind !== 'group',
  })
  const state = node.children
  const readAt = detail.data && new Date(detail.data.readAt)

  const counts = new Map<string, Node[]>()
  for (const c of under) {
    const g = c.group ?? ''
    counts.set(g, [...(counts.get(g) ?? []), c])
  }
  const eyebrow = [node.kind === 'point' ? node.group : node.kind === 'device' ? 'Device' : 'Group', node.sublabel]
    .filter(Boolean)
    .join(' ')

  return (
    <div className="detail">
      <header className="detail-head">
        <div>
          <p className="eyebrow">{eyebrow}</p>
          <h2>{node.label}</h2>
          {node.reading && (
            <p className="lede">
              <span className="mono">
                {node.reading.text}
                {node.reading.units && <span className="units"> {node.reading.units}</span>}
              </span>
              {node.reading.flags?.length ? ` · ${node.reading.flags.join(', ')}` : ''}
            </p>
          )}
        </div>
        <div className="detail-actions">
          {state.state !== 'none' && (
            <button className="button" onClick={() => reload.mutate()} disabled={state.state === 'loading' || reload.isPending}>
              Reload
            </button>
          )}
          {node.kind !== 'group' && readAt && !detail.isFetching && <span className="sub">Read at {readAt.toLocaleTimeString()}</span>}
          {detail.isFetching && <span className="spinner" aria-label="Reading" />}
          {node.kind !== 'group' && (
            <button className="button" onClick={() => detail.refetch()} disabled={detail.isFetching}>
              Read again
            </button>
          )}
        </div>
      </header>

      {node.error && <p className="banner error">{node.error}</p>}
      {state.state === 'error' && <p className="banner error">{state.error}</p>}
      {reload.isError && <p className="banner error">{reload.error.message}</p>}

      {node.facts && node.facts.length > 0 && (
        <dl className="facts">
          {node.facts.map((f) => (
            <Fragment key={f.label} label={f.label} value={f.value} mono={f.mono} />
          ))}
          {state.state !== 'none' && (
            <>
              <dt>Under it</dt>
              <dd>
                {state.state === 'unknown' && 'Not read yet'}
                {state.state === 'loading' && (state.total > 0 ? `Reading… ${state.done} of ${state.total}` : 'Reading…')}
                {state.state === 'ready' && plural(state.total, 'item')}
                {state.state === 'error' && 'Could not be read'}
              </dd>
            </>
          )}
        </dl>
      )}

      {counts.size > 0 && (
        <ul className="type-counts">
          {[...counts].map(([group, members]) => (
            <li key={group}>
              <span className={`chip ${members[0].family ?? 'other'}`}>{members.length}</span> {group || 'Items'}
            </li>
          ))}
        </ul>
      )}

      {detail.isError && <p className="banner error">Could not read the details: {detail.error.message}</p>}
      {detail.isPending && node.kind !== 'group' && <p className="sub">Reading…</p>}
      {detail.data && (
        <table className="props">
          <thead>
            <tr>
              <th>Property</th>
              <th>Value</th>
            </tr>
          </thead>
          <tbody>
            {detail.data.rows.map((r, i) => (
              <tr key={`${r.name}/${i}`}>
                <th scope="row">{r.name}</th>
                <td>
                  <RowValue row={r} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {detail.data && (
        <p className="sub foot">
          {detail.data.rows.length} {detail.data.rows.length === 1 ? 'property' : 'properties'}
        </p>
      )}
    </div>
  )
}

function Fragment({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  if (!value) return null
  return (
    <>
      <dt>{label}</dt>
      <dd className={mono ? 'mono' : undefined}>{value}</dd>
    </>
  )
}

const longList = 12

function RowValue({ row: r }: { row: DetailRow }) {
  const [all, setAll] = useState(false)
  if (r.error) return <span className="prop-error">{r.error}</span>
  if (!r.items) return <span className="prop-value">{r.value}</span>
  const items = all ? r.items : r.items.slice(0, longList)
  return (
    <>
      <ol className="prop-items">
        {items.map((item, i) => (
          <li key={i}>{item}</li>
        ))}
      </ol>
      {r.items.length > longList && (
        <button className="link" onClick={() => setAll(!all)}>
          {all ? 'Show fewer' : `Show all ${r.items.length}`}
        </button>
      )}
    </>
  )
}
