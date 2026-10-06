// Types and calls of the explorer's HTTP API (internal/api).

export interface Transport {
  name: string
  local: string
  broadcast: string
  simulated: boolean
}

export interface Adapter {
  name: string
  address: string
  broadcast: string
}

export interface Status {
  transport: Transport | null
  adapters: Adapter[]
}

export interface ObjectsState {
  state: 'idle' | 'loading' | 'ready' | 'error'
  total: number
  done: number
  error?: string
}

export interface Device {
  id: number
  address: string
  network: number
  mac?: string
  maxApdu: number
  segmentation: string
  vendorId: number
  name?: string
  vendorName?: string
  modelName?: string
  firmware?: string
  software?: string
  location?: string
  description?: string
  infoError?: string
  objects: ObjectsState
}

export interface BacnetObject {
  type: number
  typeName: string
  abbr: string
  instance: number
  name?: string
  description?: string
  value?: string
  units?: string
  flags?: string[]
  loaded: boolean
  error?: string
}

export interface ObjectList {
  rev: number
  objects: BacnetObject[]
}

export interface ObjectsEvent extends ObjectList {
  deviceId: number
  replace: boolean
}

export interface Property {
  id: number
  name: string
  index?: number
  value: string
  items?: string[]
  error?: string
}

export interface PropertyList {
  properties: Property[]
  readAt: string
}

async function request<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method: body === undefined ? 'GET' : 'POST',
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

export const api = {
  status: () => request<Status>('/api/status'),
  selectAdapter: (adapter: string) => request<Transport>('/api/transport', { adapter }),
  scan: () => request<void>('/api/discovery', {}),
  objects: (device: number) => request<ObjectList>(`/api/devices/${device}/objects`),
  reloadObjects: (device: number) => request<ObjectList>(`/api/devices/${device}/refresh`, {}),
  properties: (device: number, type: number, instance: number) =>
    request<PropertyList>(`/api/devices/${device}/objects/${type}/${instance}/properties`),
}

export const objectKey = (o: { type: number; instance: number }) => `${o.type}:${o.instance}`

export function deviceLabel(d: Device): string {
  return d.name || `Device ${d.id}`
}
