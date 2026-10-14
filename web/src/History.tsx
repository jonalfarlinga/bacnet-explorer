import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Node, type NodeID, type RegistryInfo, type Track } from './api'
import type { Nodes } from './live'
import { plural } from './Tree'

// History lists the registry files, the one in force first, shows what any
// of them holds, and puts one back in force. Restoring changes no file: the
// registry restored is written as a new one, so the list only grows.
export function History({ nodes }: { nodes: Nodes }) {
  const qc = useQueryClient()
  // Under the store's key, so that a decision makes the list stale too.
  const list = useQuery({ queryKey: ['store', 'registries'], queryFn: api.registries, retry: false })
  const [open, setOpen] = useState<string | null>(null)
  // Restoring asks twice: the first press arms the button, the second acts.
  const [armed, setArmed] = useState<string | null>(null)
  const restore = useMutation({
    mutationFn: (name: string) => api.restoreRegistry(name),
    onSuccess: () => {
      setArmed(null)
      setOpen(null)
      void qc.invalidateQueries({ queryKey: ['store'] })
    },
  })

  if (list.isError) return <p className="tree-empty sub">{list.error.message}</p>
  if (!list.data) return <p className="tree-empty sub">Reading the registry files.</p>
  if (list.data.length === 0) {
    return <p className="tree-empty sub">No registry yet. The first decision about a point writes one.</p>
  }

  return (
    <>
      {restore.isError && <p className="banner error">{restore.error.message}</p>}
      <table className="history">
        <thead>
          <tr>
            <th>Written</th>
            <th>File</th>
            <th className="num">Devices</th>
            <th className="num">Tracked</th>
            <th className="num">Sidelined</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {list.data.map((r) => [
            <tr key={r.name} className={open === r.name ? 'device-head' : undefined}>
              <td>{new Date(r.written).toLocaleString()}</td>
              <td className="mono">{r.name}</td>
              <td className="num">{r.devices}</td>
              <td className="num">{r.tracked}</td>
              <td className="num">{r.sidelined}</td>
              <td className="history-actions">
                {r.inForce && <span className="flag track-tracked">in force</span>}
                <button className="button" onClick={() => setOpen(open === r.name ? null : r.name)}>
                  {open === r.name ? 'Hide' : 'View'}
                </button>
                {!r.inForce && (
                  <button
                    className={`button${armed === r.name ? ' danger' : ''}`}
                    onClick={() => (armed === r.name ? restore.mutate(r.name) : setArmed(r.name))}
                    onBlur={() => setArmed(null)}
                    disabled={restore.isPending}
                    title="Put these decisions back in force. It is written as a new registry file; the one in force now stays in the history."
                  >
                    {armed === r.name ? 'Restore this registry?' : 'Restore'}
                  </button>
                )}
              </td>
            </tr>,
            open === r.name && (
              <tr key={`${r.name}-view`}>
                <td colSpan={6} className="history-view">
                  <RegistryView info={r} nodes={nodes} />
                </td>
              </tr>
            ),
          ])}
        </tbody>
      </table>
    </>
  )
}

// RegistryView shows the points of one registry file by device, each with
// the decision the file holds and the decision in force now, and says what
// restoring the file would change.
function RegistryView({ info, nodes }: { info: RegistryInfo; nodes: Nodes }) {
  // A registry file never changes, so it is read once.
  const file = useQuery({ queryKey: ['store', 'registry', info.name], queryFn: () => api.registry(info.name), staleTime: Infinity })

  const groups = useMemo(() => {
    const held = new Map((file.data?.nodes ?? []).map((n) => [n.id, n]))
    const byDevice = new Map<NodeID, Node[]>()
    for (const n of held.values()) {
      if (n.kind !== 'point') continue
      const dev = deviceIn(held, n)
      const points = byDevice.get(dev) ?? []
      points.push(n)
      byDevice.set(dev, points)
    }
    return [...byDevice]
      .map(([dev, points]) => ({ id: dev, device: held.get(dev), points: points.sort((a, b) => a.order - b.order) }))
      .sort((a, b) => (a.device?.label ?? '').localeCompare(b.device?.label ?? ''))
  }, [file.data])

  // What restoring would do, against the decisions as they are now.
  const change = useMemo(() => {
    const held = new Set<NodeID>()
    let decisions = 0
    let back = 0
    for (const { points } of groups) {
      for (const p of points) {
        held.add(p.id)
        const now = nodes.get(p.id)
        if (!now) back++
        else if (trackOf(now) !== trackOf(p)) decisions++
      }
    }
    let undecided = 0
    for (const n of nodes.values()) if (n.kind === 'point' && trackOf(n) !== 'new' && !held.has(n.id)) undecided++
    return { decisions, back, undecided }
  }, [groups, nodes])

  if (file.isError) return <p className="sub">{file.error.message}</p>
  if (!file.data) return <p className="sub">Reading {info.name}.</p>

  const parts = [
    change.decisions > 0 && `${plural(change.decisions, 'point')} would take the decision shown here`,
    change.back > 0 && `${plural(change.back, 'point')} removed since would come back`,
    change.undecided > 0 && `${plural(change.undecided, 'point')} decided since would be new again`,
  ].filter(Boolean)

  return (
    <>
      {!info.inForce && (
        <p className="sub history-change">
          {parts.length > 0 ? `Restoring it: ${parts.join('; ')}.` : 'Restoring it would change no decision: it holds the same ones as now.'}
        </p>
      )}
      {groups.length === 0 ? (
        <p className="sub">It holds no points.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Point</th>
              <th>Type</th>
              <th>Decision here</th>
              <th>Now</th>
            </tr>
          </thead>
          <tbody>
            {groups.map(({ id, device, points }) => [
              <tr key={id} className="device-head">
                <td colSpan={4}>
                  {device?.label ?? 'Unknown device'} <span className="sub mono">{device?.sublabel}</span>{' '}
                  <span className="sub">{plural(points.length, 'point')}</span>
                </td>
              </tr>,
              ...points.map((p) => {
                const now = nodes.get(p.id)
                return (
                  <tr key={p.id}>
                    <td>
                      {p.label} <span className="sub mono">{p.sublabel}</span>
                    </td>
                    <td>
                      <span className={`chip ${p.family ?? 'other'}`}>{p.tag}</span> <span className="sub">{p.group}</span>
                    </td>
                    <td>
                      <span className={`flag track-${trackOf(p)}`}>{trackOf(p)}</span>
                    </td>
                    <td>
                      {now ? (
                        trackOf(now) === trackOf(p) ? (
                          <span className="sub">the same</span>
                        ) : (
                          <span className={`flag track-${trackOf(now)}`}>{trackOf(now)}</span>
                        )
                      ) : (
                        <span className="flag unreached">removed</span>
                      )}
                    </td>
                  </tr>
                )
              }),
            ])}
          </tbody>
        </table>
      )}
    </>
  )
}

const trackOf = (n: Node): Track => n.track ?? 'new'

// deviceIn finds the device a point is under among the nodes of a file, or
// the last node the file has on the way up.
function deviceIn(held: ReadonlyMap<NodeID, Node>, n: Node): NodeID {
  let cur = n
  while (cur.parent) {
    const p = held.get(cur.parent)
    if (!p) return cur.parent
    if (p.kind === 'device') return p.id
    cur = p
  }
  return cur.id
}
