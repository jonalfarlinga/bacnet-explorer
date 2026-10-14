import type { Node, NodeID } from './api'
import type { Nodes } from './live'

interface RowBase {
  key: string
  depth: number
}

export type Row = RowBase &
  (
    | { kind: 'node'; node: Node; open: boolean; count: number }
    | { kind: 'group'; name: string; count: number; open: boolean }
    | { kind: 'note'; text: string; tone: 'muted' | 'error' | 'busy' }
  )

export const groupKey = (parent: NodeID, name: string) => `${parent}/g:${name}`

// What is open when the user has not said: devices start closed, so that
// nothing is read from a device until it is picked; everything else, which
// costs nothing to show, starts open.
const openByDefault = (n: Node) => n.kind !== 'device'

export interface TreeInput {
  nodes: Nodes
  // The user's own opening and closing of rows, by key.
  toggled: ReadonlyMap<string, boolean>
  filter: string
}

// matcher turns what was typed into a test of a row's fields: every word
// has to be in one of them. It is null when nothing was typed.
export function matcher(filter: string): ((...fields: (string | number | undefined)[]) => boolean) | null {
  const terms = filter.toLowerCase().split(/\s+/).filter(Boolean)
  if (terms.length === 0) return null
  return (...fields) => {
    const hay = fields.filter((f) => f !== undefined && f !== '').join('\n').toLowerCase()
    return terms.every((t) => hay.includes(t))
  }
}

const byOrder = (a: Node, b: Node) => a.order - b.order || a.label.localeCompare(b.label) || a.id.localeCompare(b.id)

// childrenOf indexes the nodes by parent, sorted as the source ordered them.
export function childrenOf(nodes: Nodes): ReadonlyMap<NodeID | '', Node[]> {
  const index = new Map<NodeID | '', Node[]>()
  for (const n of nodes.values()) {
    const key = n.parent ?? ''
    const list = index.get(key)
    if (list) list.push(n)
    else index.set(key, [n])
  }
  for (const list of index.values()) list.sort(byOrder)
  return index
}

// buildRows lays the tree out as the rows to draw, top to bottom. Children
// that share a group go under a heading of that name.
export function buildRows({ nodes, toggled, filter }: TreeInput): Row[] {
  const match = matcher(filter)
  const children = childrenOf(nodes)
  const isOpen = (key: string, node?: Node) => toggled.get(key) ?? (node ? openByDefault(node) : true)

  const nodeMatches = (n: Node) =>
    !match ||
    match(n.label, n.sublabel, n.meta, n.tag, n.group, n.reading?.text, ...(n.facts ?? []).map((f) => f.value))

  // nodeRows returns the node's row and everything under it, or nothing
  // when a filter is set and neither the node nor anything under it matches.
  const nodeRows = (n: Node, depth: number): Row[] => {
    const under = children.get(n.id) ?? []
    const self = nodeMatches(n)
    const belowRows = under.length > 0 ? listRows(under, n.id, depth + 1, self) : []
    if (match && !self && belowRows.length === 0) return []

    // Under a filter, a node is opened to show what matched inside it.
    const forced = match !== null && belowRows.length > 0
    const open = forced || isOpen(n.id, n)
    const rows: Row[] = [{ kind: 'node', key: n.id, depth, node: n, open, count: under.length }]
    if (!open || n.children.state === 'none') return rows

    rows.push(...belowRows)
    const state = n.children
    if (state.state === 'error') {
      rows.push({ kind: 'note', key: `${n.id}/error`, depth: depth + 1, tone: 'error', text: state.error ?? 'Could not read what is under it' })
    } else if (state.state === 'loading' && under.length === 0) {
      const text = state.total > 0 ? `Reading… ${state.total} items` : 'Reading…'
      rows.push({ kind: 'note', key: `${n.id}/busy`, depth: depth + 1, tone: 'busy', text })
    } else if (state.state === 'ready' && under.length === 0) {
      rows.push({ kind: 'note', key: `${n.id}/none`, depth: depth + 1, tone: 'muted', text: 'Nothing under it' })
    }
    return rows
  }

  // listRows lays out siblings, grouped where the source grouped them. With
  // parentMatches, a filter keeps every child the way it keeps the parent.
  const listRows = (list: Node[], parent: NodeID, depth: number, parentMatches: boolean): Row[] => {
    const rows: Row[] = []
    for (let i = 0; i < list.length; ) {
      const group = list[i].group
      if (!group) {
        rows.push(...nodeRows(list[i], depth))
        i++
        continue
      }
      let end = i
      while (end < list.length && list[end].group === group) end++
      const members = list.slice(i, end)
      const under = members.flatMap((m) => nodeRows(m, depth + 1))
      // A parent that matches by itself keeps all its groups unless some
      // of their members match too.
      const anyMatch = members.some(nodeMatches)
      if (match && under.length === 0 && !(parentMatches && !anyMatch)) {
        i = end
        continue
      }
      const shown = match && anyMatch ? under : match && parentMatches ? members.flatMap((m) => nodeRowsPlain(m, depth + 1)) : under
      const key = groupKey(parent, group)
      const open = (match !== null && anyMatch) || isOpen(key)
      rows.push({ kind: 'group', key, depth, name: group, count: members.length, open })
      if (open) rows.push(...shown)
      i = end
    }
    return rows
  }

  // nodeRowsPlain is nodeRows with the filter set aside, for members kept
  // because their parent matched.
  const nodeRowsPlain = (n: Node, depth: number): Row[] => [
    { kind: 'node', key: n.id, depth, node: n, open: false, count: (children.get(n.id) ?? []).length },
  ]

  return listRows(children.get('') ?? [], '', 0, false)
}

// parentOf finds the row a row hangs off: the nearest one above it that is
// less deep.
export function parentOf(rows: Row[], index: number): number {
  for (let i = index - 1; i >= 0; i--) {
    if (rows[i].depth < rows[index].depth) return i
  }
  return -1
}
