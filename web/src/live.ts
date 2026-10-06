import { useEffect, useState } from 'react'
import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import { api, objectKey, type Device, type ObjectList, type ObjectsEvent } from './api'

export const devicesKey = ['devices'] as const
export const objectsKey = (device: number) => ['objects', device] as const

// useLive keeps the query cache in step with the server's event stream, and
// reports whether the stream is connected. The stream opens with every
// device the server knows; after that come single devices and objects as the
// server learns of them. onReset runs when the server changes network, which
// makes everything shown so far stale.
export function useLive(onReset: () => void): boolean {
  const qc = useQueryClient()
  const [connected, setConnected] = useState(false)

  useEffect(() => {
    const es = new EventSource('/api/events')
    es.onopen = () => setConnected(true)
    es.onerror = () => setConnected(false) // the browser reconnects by itself

    es.addEventListener('devices', (e) => {
      qc.setQueryData<Device[]>(devicesKey, JSON.parse(e.data))
      // Events may have been missed while the stream was down.
      void qc.resetQueries({ queryKey: ['objects'] })
      void qc.invalidateQueries({ queryKey: ['status'] })
    })
    es.addEventListener('device', (e) => {
      const device: Device = JSON.parse(e.data)
      qc.setQueryData<Device[]>(devicesKey, (list = []) => {
        const i = list.findIndex((d) => d.id === device.id)
        if (i < 0) return [...list, device].sort((a, b) => a.id - b.id)
        const next = list.slice()
        next[i] = device
        return next
      })
    })
    es.addEventListener('objects', (e) => applyObjects(qc, JSON.parse(e.data)))
    es.addEventListener('reset', () => {
      qc.setQueryData<Device[]>(devicesKey, [])
      qc.removeQueries({ queryKey: ['objects'] })
      qc.removeQueries({ queryKey: ['properties'] })
      void qc.invalidateQueries({ queryKey: ['status'] })
      onReset()
    })
    return () => es.close()
  }, [qc])

  return connected
}

// applyObjects merges an event into the cached objects of its device. Each
// event carries the revision it brings the list to; one that does not follow
// the cached revision means something was missed, and the list is fetched
// again instead.
function applyObjects(qc: QueryClient, ev: ObjectsEvent) {
  const key = objectsKey(ev.deviceId)
  const cached = qc.getQueryData<ObjectList>(key)
  if (ev.replace) {
    if (!cached || ev.rev > cached.rev) qc.setQueryData<ObjectList>(key, { rev: ev.rev, objects: ev.objects })
    return
  }
  if (!cached) return // nobody has asked for this device's objects yet
  if (ev.rev <= cached.rev) return
  if (ev.rev !== cached.rev + 1) {
    void qc.invalidateQueries({ queryKey: key })
    return
  }
  const changed = new Map(ev.objects.map((o) => [objectKey(o), o]))
  qc.setQueryData<ObjectList>(key, {
    rev: ev.rev,
    objects: cached.objects.map((o) => changed.get(objectKey(o)) ?? o),
  })
}

export function useDevices(): Device[] {
  // The event stream fills this; there is nothing to fetch.
  const { data } = useQuery<Device[]>({ queryKey: devicesKey, queryFn: () => [], enabled: false })
  return data ?? []
}

// objectsQuery reads a device's objects. The first read makes the server
// start discovering them; the rest arrive as events.
export function objectsQuery(qc: QueryClient, device: number) {
  return {
    queryKey: objectsKey(device),
    queryFn: async (): Promise<ObjectList> => keepNewer(qc, device, await api.objects(device)),
  }
}

// keepNewer guards against a snapshot that events have already overtaken.
export function keepNewer(qc: QueryClient, device: number, snapshot: ObjectList): ObjectList {
  const cached = qc.getQueryData<ObjectList>(objectsKey(device))
  return cached && cached.rev > snapshot.rev ? cached : snapshot
}
