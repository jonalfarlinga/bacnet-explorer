import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Node, type NodeID, type Track } from './api'
import type { Nodes } from './live'
import { plural } from './Tree'
import { childrenOf } from './treeModel'

const tracks: { track: Track; label: string }[] = [
  { track: 'new', label: 'New' },
  { track: 'tracked', label: 'Tracked' },
  { track: 'sidelined', label: 'Sidelined' },
]

// Admin lists the points by the decision taken about them, grouped by
// device, and takes decisions in batch. Nothing here reads a device: it is
// about what the data consumers get, not what the explorer shows.
export function Admin({ nodes, onClose }: { nodes: Nodes; onClose: () => void }) {
  const qc = useQueryClient()
  const [tab, setTab] = useState<Track>('new')
  const [picked, setPicked] = useState<ReadonlySet<NodeID>>(new Set())
  const store = useQuery({ queryKey: ['store'], queryFn: api.store, retry: false })
  const classify = useMutation({
    mutationFn: ({ ids, track }: { ids: NodeID[]; track: Track }) => api.classify(ids, track),
    onSuccess: () => {
      setPicked(new Set())
      void qc.invalidateQueries({ queryKey: ['store'] })
    },
  })
  // Removing asks twice: the first press arms the button, the second acts.
  const [armed, setArmed] = useState(false)
  const remove = useMutation({
    mutationFn: (ids: NodeID[]) => api.remove(ids),
    onSuccess: () => {
      setPicked(new Set())
      setArmed(false)
      void qc.invalidateQueries({ queryKey: ['store'] })
    },
  })
  const walk = useQuery({ queryKey: ['walk'], queryFn: api.walk, refetchInterval: 5000 })

  const children = useMemo(() => childrenOf(nodes), [nodes])
  // The points of each device, by the device, for the tab's decision.
  const groups = useMemo(() => {
    const byDevice = new Map<NodeID, Node[]>()
    for (const n of nodes.values()) {
      if (n.kind !== 'point' || (n.track ?? 'new') !== tab) continue
      const dev = deviceOf(nodes, n)
      const list = byDevice.get(dev) ?? []
      list.push(n)
      byDevice.set(dev, list)
    }
    return [...byDevice]
      .map(([dev, points]) => ({ device: nodes.get(dev), points: points.sort((a, b) => a.order - b.order) }))
      .sort((a, b) => (a.device?.label ?? '').localeCompare(b.device?.label ?? ''))
  }, [nodes, tab])
  const counts = useMemo(() => {
    const c = { new: 0, tracked: 0, sidelined: 0 }
    for (const n of nodes.values()) if (n.kind === 'point') c[n.track ?? 'new']++
    return c
  }, [nodes])
  const shown = groups.flatMap((g) => g.points.map((p) => p.id))
  const pickedShown = shown.filter((id) => picked.has(id))

  const toggle = (ids: NodeID[], on: boolean) =>
    setPicked((prev) => {
      const next = new Set(prev)
      for (const id of ids) on ? next.add(id) : next.delete(id)
      return next
    })
  const decide = (track: Track) => {
    if (pickedShown.length > 0) classify.mutate({ ids: pickedShown, track })
  }

  return (
    <main className="admin">
      <div className="admin-bar">
        {tracks.map((t) => (
          <button key={t.track} className={`tab${tab === t.track ? ' active' : ''}`} onClick={() => setTab(t.track)}>
            {t.label} <span className="sub">{counts[t.track]}</span>
          </button>
        ))}
        <span className="spacer" />
        <span className="sub">{pickedShown.length > 0 ? `${pickedShown.length} picked` : 'Pick points, then decide'}</span>
        <button className="button" onClick={() => toggle(shown, pickedShown.length !== shown.length)} disabled={shown.length === 0}>
          {pickedShown.length === shown.length && shown.length > 0 ? 'Pick none' : 'Pick all'}
        </button>
        {tab !== 'tracked' && (
          <button className="button primary" onClick={() => decide('tracked')} disabled={pickedShown.length === 0 || classify.isPending}>
            Track
          </button>
        )}
        {tab !== 'sidelined' && (
          <button className="button" onClick={() => decide('sidelined')} disabled={pickedShown.length === 0 || classify.isPending}>
            Sideline
          </button>
        )}
        <button
          className={`button${armed ? ' danger' : ''}`}
          onClick={() => (armed ? remove.mutate(pickedShown) : setArmed(true))}
          onBlur={() => setArmed(false)}
          disabled={pickedShown.length === 0 || remove.isPending}
          title="Forget the picked points: for ones that have moved or are gone. A point the device still has comes back as new when the device is read."
        >
          {armed ? `Remove ${pickedShown.length}?` : 'Remove'}
        </button>
        <button className="button" onClick={onClose}>
          Back
        </button>
      </div>
      {classify.isError && <p className="banner error">{classify.error.message}</p>}
      {remove.isError && <p className="banner error">{remove.error.message}</p>}
      {store.isError && <p className="banner error">The explorer runs without a store: decisions are not kept.</p>}

      <div className="admin-list">
        {groups.length === 0 ? (
          <p className="tree-empty sub">
            {tab === 'new' ? 'No undecided points. Open devices in the explorer to find their points.' : `No ${tab} points.`}
          </p>
        ) : (
          <table>
            <thead>
              <tr>
                <th />
                <th>Point</th>
                <th>Type</th>
                <th>Value</th>
                <th>Status</th>
              </tr>
            </thead>
            <tbody>
              {groups.map(({ device, points }) => {
                const ids = points.map((p) => p.id)
                const all = ids.every((id) => picked.has(id))
                const some = ids.some((id) => picked.has(id))
                return [
                  <tr key={device?.id ?? 'none'} className="device-head">
                    <td>
                      <input
                        type="checkbox"
                        checked={all}
                        ref={(el) => {
                          if (el) el.indeterminate = some && !all
                        }}
                        onChange={(e) => toggle(ids, e.target.checked)}
                        aria-label={`Pick every point of ${device?.label}`}
                      />
                    </td>
                    <td colSpan={2}>
                      {device?.label ?? 'Unknown device'} <span className="sub mono">{device?.sublabel}</span>{' '}
                      <span className="sub">{plural(points.length, 'point')}</span>
                    </td>
                    <td colSpan={2}>
                      <span className="sub mono">{device?.meta}</span>{' '}
                      {device && !device.reached && <span className="flag unreached">not responding</span>}
                      {device && (children.get(device.id)?.length ?? 0) === 0 && (
                        <span className="sub">its points have not been read this run</span>
                      )}
                    </td>
                  </tr>,
                  ...points.map((p) => (
                    <tr key={p.id}>
                      <td>
                        <input type="checkbox" checked={picked.has(p.id)} onChange={(e) => toggle([p.id], e.target.checked)} aria-label={`Pick ${p.label}`} />
                      </td>
                      <td>
                        {p.label} <span className="sub mono">{p.sublabel}</span>
                      </td>
                      <td>
                        <span className={`chip ${p.family ?? 'other'}`}>{p.tag}</span> <span className="sub">{p.group}</span>
                      </td>
                      <td className="mono">
                        {p.reading?.text}
                        {p.reading?.units && <span className="units"> {p.reading.units}</span>}
                      </td>
                      <td>
                        {!p.reached && <span className="flag unreached">not responding</span>}
                        {p.track && p.track !== 'new' && <span className={`flag track-${p.track}`}>{p.track}</span>}
                        {p.reading?.flags?.map((f) => (
                          <span key={f} className="flag alarm">
                            {f}
                          </span>
                        ))}
                      </td>
                    </tr>
                  )),
                ]
              })}
            </tbody>
          </table>
        )}
      </div>
      <p className="admin-foot">
        {store.data
          ? `Store ${store.data.dir}: ${store.data.latest ? `registry ${store.data.latest}` : 'no decision yet'}, ${plural(store.data.registries, 'registry file')}. Undecided points are in temp.json; each decision writes a new registry file.`
          : ''}
        <br />
        {walk.data && !walk.data.enabled && 'The background walk is off.'}
        {walk.data?.enabled &&
          `Background walk, ${walk.data.pointsPerMinute} points a minute: ${
            walk.data.current
              ? `reading ${walk.data.currentLabel}`
              : walk.data.waiting ?? 'idle'
          }; ${plural(walk.data.done, 'device')} walked this run, ${walk.data.due} due.${walk.data.lastError ? ` Last error: ${walk.data.lastError}` : ''}`}
      </p>
    </main>
  )
}

// deviceOf finds the device a point belongs to, or the point itself when
// it is a root.
function deviceOf(nodes: Nodes, n: Node): NodeID {
  let cur: Node | undefined = n
  while (cur?.parent) {
    const p = nodes.get(cur.parent)
    if (!p) break
    if (p.kind === 'device') return p.id
    cur = p
  }
  return cur?.parent ?? n.id
}
