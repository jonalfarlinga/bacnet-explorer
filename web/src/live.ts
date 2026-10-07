import { useEffect, useState } from 'react'
import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import { api, type ChildrenEvent, type ChildrenList, type Node, type NodeID } from './api'

export const nodesKey = ['nodes'] as const
export const revKey = (id: NodeID) => ['rev', id] as const

// Nodes is everything the UI knows, by ID. The tree is built from it: a
// node's children are the nodes whose parent it is.
export type Nodes = ReadonlyMap<NodeID, Node>

// useLive keeps the query cache in step with the server's event stream, and
// reports whether the stream is connected. The stream opens with every node
// the server knows; after that come single nodes and lists of children as
// the server learns of them. onReset runs when a source starts over, which
// makes everything shown so far stale.
export function useLive(onReset: () => void): boolean {
  const qc = useQueryClient()
  const [connected, setConnected] = useState(false)

  useEffect(() => {
    const es = new EventSource('/api/events')
    es.onopen = () => setConnected(true)
    es.onerror = () => setConnected(false) // the browser reconnects by itself

    es.addEventListener('nodes', (e) => {
      const list: Node[] = JSON.parse(e.data)
      qc.setQueryData<Nodes>(nodesKey, new Map(list.map((n) => [n.id, n])))
      // Events may have been missed while the stream was down.
      qc.removeQueries({ queryKey: ['rev'] })
      void qc.invalidateQueries({ queryKey: ['sources'] })
    })
    es.addEventListener('node', (e) => {
      const node: Node = JSON.parse(e.data)
      qc.setQueryData<Nodes>(nodesKey, (nodes = new Map()) => new Map(nodes).set(node.id, node))
    })
    es.addEventListener('children', (e) => applyChildren(qc, JSON.parse(e.data)))
    es.addEventListener('remove', (e) => {
      const id: NodeID = JSON.parse(e.data)
      qc.setQueryData<Nodes>(nodesKey, (nodes = new Map()) => {
        const next = new Map(nodes)
        dropUnder(next, id, false)
        next.delete(id)
        return next
      })
    })
    es.addEventListener('source', () => {
      void qc.invalidateQueries({ queryKey: ['sources'] })
    })
    es.addEventListener('reset', () => {
      qc.setQueryData<Nodes>(nodesKey, new Map())
      qc.removeQueries({ queryKey: ['rev'] })
      qc.removeQueries({ queryKey: ['detail'] })
      void qc.invalidateQueries({ queryKey: ['sources'] })
      onReset()
    })
    return () => es.close()
  }, [qc, onReset])

  return connected
}

// dropUnder removes what is under a node from the map, with everything
// under that: only the children that came in a list, with listedOnly, or
// every child. A list replaces only listed children; what the source
// announced on its own, such as a network behind a router, stays.
function dropUnder(nodes: Map<NodeID, Node>, parent: NodeID, listedOnly: boolean) {
  for (const n of [...nodes.values()]) {
    if (n.parent === parent && (n.listed || !listedOnly)) {
      dropUnder(nodes, n.id, false)
      nodes.delete(n.id)
    }
  }
}

// applyChildren merges an event into the cached nodes. Each event carries
// the revision it brings the parent's children to; one that does not follow
// the cached revision means something was missed, and the list is fetched
// again instead.
function applyChildren(qc: QueryClient, ev: ChildrenEvent) {
  const cached = qc.getQueryData<number>(revKey(ev.parent))
  if (!ev.replace && cached !== undefined && ev.rev !== cached + 1) {
    void qc.invalidateQueries({ queryKey: revKey(ev.parent) })
    return
  }
  if (!ev.replace && cached === undefined) {
    // Nobody has asked for this node's children yet; the list is fetched
    // when they do.
    return
  }
  qc.setQueryData<number>(revKey(ev.parent), ev.rev)
  qc.setQueryData<Nodes>(nodesKey, (nodes = new Map()) => {
    const next = new Map(nodes)
    if (ev.replace) dropUnder(next, ev.parent, true)
    for (const n of ev.nodes) next.set(n.id, n)
    return next
  })
}

export function useNodes(): Nodes {
  // The event stream fills this; there is nothing to fetch.
  const { data } = useQuery<Nodes>({ queryKey: nodesKey, queryFn: () => new Map(), enabled: false })
  return data ?? new Map()
}

// childrenQuery reads a node's children. The first read makes the server
// start reading them; the rest arrive as events. It resolves to the
// revision, and puts the nodes in the map.
export function childrenQuery(qc: QueryClient, id: NodeID) {
  return {
    queryKey: revKey(id),
    queryFn: async (): Promise<number> => takeChildren(qc, id, await api.children(id)),
  }
}

// takeChildren puts a fetched list in the map, unless events have already
// taken the node's children past it.
export function takeChildren(qc: QueryClient, id: NodeID, list: ChildrenList): number {
  const cached = qc.getQueryData<number>(revKey(id))
  if (cached !== undefined && cached > list.rev) return cached
  qc.setQueryData<Nodes>(nodesKey, (nodes = new Map()) => {
    const next = new Map(nodes)
    dropUnder(next, id, true)
    for (const n of list.nodes) next.set(n.id, n)
    return next
  })
  return list.rev
}
