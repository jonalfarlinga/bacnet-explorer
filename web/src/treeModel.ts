import { objectKey, type BacnetObject, type Device, type ObjectList } from './api'

export type Selection =
  | { kind: 'device'; device: number }
  | { kind: 'object'; device: number; type: number; instance: number }

interface RowBase {
  key: string
  depth: number
}

export type Row = RowBase &
  (
    | { kind: 'device'; device: Device; open: boolean }
    // A router the devices behind it name, but which has not answered itself.
    | { kind: 'router'; address: string; open: boolean }
    | { kind: 'network'; network: number; count: number; open: boolean }
    | { kind: 'group'; typeName: string; count: number; open: boolean }
    | { kind: 'object'; device: number; object: BacnetObject }
    | { kind: 'note'; text: string; tone: 'muted' | 'error' | 'busy' }
  )

export const deviceRowKey = (id: number) => `d${id}`
export const objectRowKey = (device: number, o: { type: number; instance: number }) =>
  `d${device}/o${objectKey(o)}`

// What is open when the user has not said: devices start closed, so that
// nothing is read from a device until it is picked.
const openByDefault = { device: false, router: true, network: true, group: true }

export interface TreeInput {
  devices: Device[]
  objects: (device: number) => ObjectList | undefined
  // The user's own opening and closing of rows, by key.
  toggled: ReadonlyMap<string, boolean>
  filter: string
}

function matcher(filter: string): ((...fields: (string | number | undefined)[]) => boolean) | null {
  const terms = filter.toLowerCase().split(/\s+/).filter(Boolean)
  if (terms.length === 0) return null
  return (...fields) => {
    const hay = fields.filter((f) => f !== undefined && f !== '').join('\n').toLowerCase()
    return terms.every((t) => hay.includes(t))
  }
}

export function titleCase(typeName: string): string {
  return typeName
    .split('-')
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join(' ')
}

// buildRows lays the tree out as the rows to draw, top to bottom. Devices
// with an address of their own are at the top level. A device behind a
// router hangs off the router, under its network; a device's objects hang
// off the device, grouped by type.
export function buildRows({ devices, objects, toggled, filter }: TreeInput): Row[] {
  const match = matcher(filter)
  const isOpen = (key: string, kind: keyof typeof openByDefault) => toggled.get(key) ?? openByDefault[kind]

  const local = devices.filter((d) => d.network === 0)
  const routed = new Map<string, Device[]>() // by router address
  for (const d of devices) {
    if (d.network === 0) continue
    const list = routed.get(d.address) ?? []
    list.push(d)
    routed.set(d.address, list)
  }

  // deviceRows returns the device's row and everything under it, or nothing
  // when a filter is set and neither the device nor anything under it matches.
  const deviceRows = (d: Device, depth: number): Row[] => {
    const key = deviceRowKey(d.id)
    const deviceMatches =
      !match || match(d.name, d.id, `#${d.id}`, d.address, d.mac, d.vendorName, d.modelName, d.description, d.location)
    const list = objects(d.id)

    let shown = list?.objects ?? []
    let narrowed = false
    if (match && list) {
      const hits = shown.filter((o) =>
        match(o.name, o.description, o.typeName, `${o.typeName}:${o.instance}`, `${o.abbr}${o.instance}`, `${o.abbr}:${o.instance}`),
      )
      // A device that matches by itself keeps all its objects unless some of
      // them match too.
      if (hits.length > 0 || !deviceMatches) {
        shown = hits
        narrowed = true
      }
    }

    const behind = d.network === 0 ? networkRows(routed.get(d.address) ?? [], key, depth + 1) : []
    if (match && !deviceMatches && behind.length === 0 && !(narrowed && shown.length > 0)) return []

    // Under a filter, a device is opened to show what matched inside it.
    const forced = match !== null && ((narrowed && shown.length > 0) || behind.length > 0)
    const open = forced || isOpen(key, 'device')
    const rows: Row[] = [{ kind: 'device', key, depth, device: d, open }]
    if (!open) return rows

    rows.push(...behind)
    const state = d.objects
    if (state.state === 'error') {
      rows.push({ kind: 'note', key: `${key}/error`, depth: depth + 1, tone: 'error', text: state.error ?? 'Could not read the objects' })
    } else if (!list || (state.state === 'loading' && list.objects.length === 0)) {
      const text = state.total > 0 ? `Reading the object list… ${state.total} objects` : 'Reading the object list…'
      rows.push({ kind: 'note', key: `${key}/busy`, depth: depth + 1, tone: 'busy', text })
    } else if (list.objects.length === 0) {
      rows.push({ kind: 'note', key: `${key}/none`, depth: depth + 1, tone: 'muted', text: 'No objects' })
    }

    for (let i = 0; i < shown.length; ) {
      let end = i
      while (end < shown.length && shown[end].type === shown[i].type) end++
      const groupKey = `${key}/t${shown[i].type}`
      const groupOpen = narrowed || isOpen(groupKey, 'group')
      rows.push({
        kind: 'group', key: groupKey, depth: depth + 1,
        typeName: shown[i].typeName, count: end - i, open: groupOpen,
      })
      if (groupOpen) {
        for (const o of shown.slice(i, end)) {
          rows.push({ kind: 'object', key: objectRowKey(d.id, o), depth: depth + 2, device: d.id, object: o })
        }
      }
      i = end
    }
    return rows
  }

  // networkRows lays out the devices behind one router, by network.
  const networkRows = (behind: Device[], parentKey: string, depth: number): Row[] => {
    const rows: Row[] = []
    const networks = [...new Set(behind.map((d) => d.network))].sort((a, b) => a - b)
    for (const network of networks) {
      const key = `${parentKey}/n${network}`
      const members = behind.filter((d) => d.network === network)
      const under = members.flatMap((d) => deviceRows(d, depth + 1))
      if (match && under.length === 0) continue
      const open = match !== null || isOpen(key, 'network')
      rows.push({ kind: 'network', key, depth, network, count: members.length, open })
      if (open) rows.push(...under)
    }
    return rows
  }

  const rows: Row[] = []
  for (const d of local) rows.push(...deviceRows(d, 0))

  // Routers that only the devices behind them have told us about.
  const known = new Set(local.map((d) => d.address))
  for (const address of [...routed.keys()].filter((a) => !known.has(a)).sort()) {
    const key = `r${address}`
    const under = networkRows(routed.get(address)!, key, 1)
    if (match && under.length === 0) continue
    const open = match !== null || isOpen(key, 'router')
    rows.push({ kind: 'router', key, depth: 0, address, open })
    if (open) rows.push(...under)
  }
  return rows
}

// parentOf finds the row a row hangs off: the nearest one above it that is
// less deep.
export function parentOf(rows: Row[], index: number): number {
  for (let i = index - 1; i >= 0; i--) {
    if (rows[i].depth < rows[index].depth) return i
  }
  return -1
}
