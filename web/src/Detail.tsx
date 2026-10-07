import { useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, deviceLabel, type BacnetObject, type Device, type ObjectList, type Property } from './api'
import { keepNewer, objectsKey } from './live'
import { family } from './Tree'
import { titleCase } from './treeModel'

export function EmptyDetail({ devices }: { devices: number }) {
  return (
    <div className="empty">
      <h2>{devices === 0 ? 'Listening for devices' : 'Pick a device'}</h2>
      <p>
        {devices === 0
          ? 'A Who-Is has gone out. Devices appear on the left as they answer; Scan sends another.'
          : 'Picking a device reads its object list and hangs the objects under it. Pick an object to read its properties.'}
      </p>
    </div>
  )
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  if (children === undefined || children === null || children === '') return null
  return (
    <>
      <dt>{label}</dt>
      <dd>{children}</dd>
    </>
  )
}

interface DeviceDetailProps {
  device: Device
  // The device this one is reached through, if it is known.
  router?: Device
  objects?: ObjectList
}

export function DeviceDetail({ device, router, objects }: DeviceDetailProps) {
  const qc = useQueryClient()
  const reload = useMutation({
    mutationFn: () => api.reloadObjects(device.id),
    onSuccess: (snapshot) => qc.setQueryData(objectsKey(device.id), keepNewer(qc, device.id, snapshot)),
  })
  const state = device.objects

  const counts = new Map<string, number>()
  for (const o of objects?.objects ?? []) counts.set(o.typeName, (counts.get(o.typeName) ?? 0) + 1)

  return (
    <div className="detail">
      <header className="detail-head">
        <div>
          <p className="eyebrow">Device {device.id}</p>
          <h2>{deviceLabel(device)}</h2>
          {device.description && <p className="lede">{device.description}</p>}
        </div>
        <button className="button" onClick={() => reload.mutate()} disabled={state.state === 'loading' || reload.isPending}>
          Reload objects
        </button>
      </header>

      {device.infoError && <p className="banner error">Could not read the device's name and model: {device.infoError}</p>}
      {state.state === 'error' && <p className="banner error">{state.error}</p>}
      {reload.isError && <p className="banner error">{reload.error.message}</p>}

      <dl className="facts">
        <Fact label="Address">
          {device.network === 0 ? (
            <span className="mono">{device.address}</span>
          ) : (
            <>
              Network {device.network}, MAC <span className="mono">{device.mac}</span>, through{' '}
              {router ? `${deviceLabel(router)} at ` : 'the router at '}
              <span className="mono">{device.address}</span>
            </>
          )}
          {device.added && (
            <>
              {' '}
              <span className="sub" title="This device did not answer the Who-Is: it was added by its address and read directly.">
                (added by address)
              </span>
            </>
          )}
        </Fact>
        <Fact label="Vendor">
          {device.vendorName ? `${device.vendorName} (${device.vendorId})` : `Vendor ${device.vendorId}`}
        </Fact>
        <Fact label="Model">{device.modelName}</Fact>
        <Fact label="Firmware">{device.firmware}</Fact>
        <Fact label="Application software">{device.software}</Fact>
        <Fact label="Location">{device.location}</Fact>
        <Fact label="Max APDU">{device.maxApdu} bytes</Fact>
        <Fact label="Segmentation">{device.segmentation}</Fact>
        <Fact label="Objects">
          {state.state === 'idle' && 'Not read yet'}
          {state.state === 'loading' &&
            (state.total > 0 ? `Reading… ${state.done} of ${state.total}` : 'Reading the object list…')}
          {state.state === 'ready' && state.total}
          {state.state === 'error' && 'Could not be read'}
        </Fact>
      </dl>

      {counts.size > 0 && (
        <ul className="type-counts">
          {[...counts].map(([typeName, n]) => (
            <li key={typeName}>
              <span className={`chip ${family({ typeName })}`}>{n}</span> {titleCase(typeName)}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

const longList = 12

function PropertyValue({ property: p }: { property: Property }) {
  const [all, setAll] = useState(false)
  if (p.error) return <span className="prop-error">{p.error}</span>
  if (!p.items) return <span className="prop-value">{p.value}</span>
  const items = all ? p.items : p.items.slice(0, longList)
  return (
    <>
      <ol className="prop-items">
        {items.map((item, i) => (
          <li key={i}>{item}</li>
        ))}
      </ol>
      {p.items.length > longList && (
        <button className="link" onClick={() => setAll(!all)}>
          {all ? 'Show fewer' : `Show all ${p.items.length}`}
        </button>
      )}
    </>
  )
}

interface ObjectDetailProps {
  device: Device
  type: number
  instance: number
  // What the tree knows of the object, if its device's objects are loaded.
  object?: BacnetObject
}

export function ObjectDetail({ device, type, instance, object }: ObjectDetailProps) {
  const query = useQuery({
    queryKey: ['properties', device.id, type, instance],
    queryFn: () => api.properties(device.id, type, instance),
    // An object picked again is read again: the values are live.
    staleTime: 0,
    gcTime: 0,
  })
  const typeName = object?.typeName ?? `type ${type}`
  const readAt = query.data && new Date(query.data.readAt)

  return (
    <div className="detail">
      <header className="detail-head">
        <div>
          <p className="eyebrow">
            {titleCase(typeName)} {instance} · {deviceLabel(device)}
          </p>
          <h2>{object?.name || `${typeName}:${instance}`}</h2>
          {object?.description && <p className="lede">{object.description}</p>}
        </div>
        <div className="detail-actions">
          {readAt && !query.isFetching && <span className="sub">Read at {readAt.toLocaleTimeString()}</span>}
          {query.isFetching && <span className="spinner" aria-label="Reading" />}
          <button className="button" onClick={() => query.refetch()} disabled={query.isFetching}>
            Read again
          </button>
        </div>
      </header>

      {query.isError && <p className="banner error">Could not read the properties: {query.error.message}</p>}
      {query.isPending && <p className="sub">Reading the properties…</p>}

      {query.data && (
        <table className="props">
          <thead>
            <tr>
              <th>Property</th>
              <th>Value</th>
            </tr>
          </thead>
          <tbody>
            {query.data.properties.map((p) => (
              <tr key={`${p.id}/${p.index ?? ''}`}>
                <th scope="row">
                  {p.name}
                  {p.index !== undefined && ` [${p.index}]`}
                  <span className="prop-id">{p.id}</span>
                </th>
                <td>
                  <PropertyValue property={p} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {query.data && (
        <p className="sub foot">
          {query.data.properties.length} {query.data.properties.length === 1 ? 'property' : 'properties'}
        </p>
      )}
    </div>
  )
}
