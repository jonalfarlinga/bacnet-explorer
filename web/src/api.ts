// Types and calls of the explorer's HTTP API (internal/api). The UI knows
// nodes and sources; what they are made of is the source's business.

export type NodeID = string
export type Kind = 'device' | 'group' | 'point'
export type Family = 'analog' | 'binary' | 'multistate' | 'text' | 'other'
export type Quality = 'good' | 'stale' | 'comm-fail' | 'error'
// The admin's decision about a point: new until decided, then tracked or
// sidelined. The explorer shows every point either way.
export type Track = 'new' | 'tracked' | 'sidelined'

export interface Fact {
  label: string
  value: string
  mono?: boolean
}

export interface Reading {
  text: string
  number?: number
  units?: string
  flags?: string[]
  quality: Quality
  at: string
}

export interface ChildrenState {
  state: 'none' | 'unknown' | 'loading' | 'ready' | 'error'
  total: number
  done: number
  error?: string
}

export interface Node {
  id: NodeID
  parent?: NodeID
  source: string
  kind: Kind
  label: string
  sublabel?: string
  meta?: string
  tag?: string
  family?: Family
  group?: string
  order: number
  facts?: Fact[]
  reading?: Reading
  children: ChildrenState
  track?: Track
  // Set once a source has heard from or read the node in this run. A node
  // loaded from the store starts unreached.
  reached: boolean
  listed?: boolean
  pending?: boolean
  error?: string
}

export interface ChildrenList {
  rev: number
  nodes: Node[]
}

export interface ChildrenEvent extends ChildrenList {
  parent: NodeID
  replace: boolean
}

export interface DetailRow {
  name: string
  value: string
  items?: string[]
  error?: string
}

export interface Detail {
  rows: DetailRow[]
  readAt: string
}

export interface Option {
  value: string
  label: string
  hint?: string
}

export interface Field {
  key: string
  label: string
  hint?: string
  type: 'text' | 'number' | 'select'
  options?: Option[]
  required?: boolean
  default?: string
}

export interface SourceStatus {
  open: boolean
  summary?: string
  lines?: Fact[]
}

export interface Source {
  kind: string
  title: string
  blurb?: string
  settings: Field[]
  addForm: Field[]
  canDiscover: boolean
  discoverHint?: string
  status: SourceStatus
}

export type Values = Record<string, string>

export interface StoreInfo {
  dir: string
  latest?: string
  registries: number
}

async function request<T>(path: string, body?: unknown, method?: string): Promise<T> {
  const res = await fetch(path, {
    method: method ?? (body === undefined ? 'GET' : 'POST'),
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) {
    const text = await res.text()
    let message = text.trim() || res.statusText
    try {
      message = JSON.parse(text).error ?? message
    } catch {
      // The body was not JSON: use it as it is.
    }
    throw new Error(message)
  }
  return res.status === 204 ? (undefined as T) : res.json()
}

const withID = (path: string, id: NodeID) => `${path}?id=${encodeURIComponent(id)}`

export const api = {
  sources: () => request<Source[]>('/api/sources'),
  openSource: (kind: string, settings: Values) => request<Source[]>('/api/sources', { kind, settings }),
  closeSource: (kind: string) => request<void>(`/api/sources/${encodeURIComponent(kind)}`, undefined, 'DELETE'),
  discover: (kind: string) => request<void>(`/api/sources/${encodeURIComponent(kind)}/discover`, {}),
  addNode: (kind: string, values: Values) => request<Node>(`/api/sources/${encodeURIComponent(kind)}/nodes`, { values }),
  nodes: () => request<Node[]>('/api/nodes'),
  children: (id: NodeID) => request<ChildrenList>(withID('/api/node/children', id)),
  refresh: (id: NodeID) => request<ChildrenList>(withID('/api/node/refresh', id), {}),
  detail: (id: NodeID) => request<Detail>(withID('/api/node/detail', id)),
  store: () => request<StoreInfo>('/api/store'),
  classify: (ids: NodeID[], track: Track) => request<Node[]>('/api/classify', { ids, track }),
}
