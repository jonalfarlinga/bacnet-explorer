import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Adapter, type Status } from './api'
import { DeviceDetail, EmptyDetail, ObjectDetail } from './Detail'
import { objectsQuery, useDevices, useLive } from './live'
import { plural, Tree } from './Tree'
import { buildRows, type Selection } from './treeModel'

// The adapter picked last time, so that the next visit starts scanning it
// without asking.
const adapterKey = 'bacnet-explorer.adapter'

function remembered(): string | null {
  try {
    return localStorage.getItem(adapterKey)
  } catch {
    return null
  }
}

function remember(adapter: string) {
  try {
    localStorage.setItem(adapterKey, adapter)
  } catch {
    // Without storage the adapter is asked for each time.
  }
}

export function App() {
  const qc = useQueryClient()
  const [selection, setSelection] = useState<Selection | null>(null)
  const [toggled, setToggled] = useState<ReadonlyMap<string, boolean>>(new Map())
  const [filter, setFilter] = useState('')

  const connected = useLive(
    useCallback(() => {
      setSelection(null)
      setToggled(new Map())
    }, []),
  )
  const status = useQuery({ queryKey: ['status'], queryFn: api.status })
  const devices = useDevices()

  const select = useMutation({
    mutationFn: api.selectAdapter,
    onSuccess: (_, adapter) => {
      remember(adapter)
      void qc.invalidateQueries({ queryKey: ['status'] })
    },
  })

  // Go back to the adapter used last time, once, if the server has none.
  const resumed = useRef(false)
  useEffect(() => {
    if (resumed.current || !status.data) return
    resumed.current = true
    const last = remembered()
    if (!status.data.transport && last && status.data.adapters.some((a) => a.name === last)) select.mutate(last)
  }, [status.data, select])

  // A device's objects are read while its row is open.
  const openDevices = useMemo(
    () => devices.filter((d) => toggled.get(`d${d.id}`)).map((d) => d.id),
    [devices, toggled],
  )
  const objectQueries = useQueries({ queries: openDevices.map((id) => objectsQuery(qc, id)) })
  // useQueries returns a new array each time, so the map is rebuilt only
  // when some list has actually been updated.
  const updated = objectQueries.map((q) => q.dataUpdatedAt).join()
  const objects = useMemo(
    () => new Map(openDevices.map((id, i) => [id, objectQueries[i]?.data])),
    [openDevices, updated],
  )

  const rows = useMemo(
    () => buildRows({ devices, objects: (id) => objects.get(id), toggled, filter }),
    [devices, objects, toggled, filter],
  )
  const behind = useMemo(() => {
    const counts = new Map<string, number>()
    for (const d of devices) if (d.network !== 0) counts.set(d.address, (counts.get(d.address) ?? 0) + 1)
    return counts
  }, [devices])

  const onToggle = useCallback((key: string, open: boolean) => {
    setToggled((prev) => new Map(prev).set(key, open))
  }, [])

  const transport = status.data?.transport ?? null
  const selectedDevice = selection && devices.find((d) => d.id === selection.device)

  return (
    <div className="app">
      <header className="bar">
        <h1>
          <svg viewBox="0 0 16 16" aria-hidden>
            <rect width="16" height="16" rx="3" fill="currentColor" />
            <path d="M4 4h3v3H4zM9 9h3v3H9zM5.5 7v3.5H9" fill="none" stroke="var(--bar-bg)" strokeWidth="1.4" />
          </svg>
          BACnet Explorer
        </h1>
        {status.data && transport && (
          <TransportPicker status={status.data} busy={select.isPending} onPick={(a) => select.mutate(a)} />
        )}
        <span className="pill" title="The explorer only sends Who-Is, ReadProperty and ReadPropertyMultiple. It never writes to a device.">
          read-only
        </span>
        <span className="spacer" />
        {!connected && <span className="flag fault">reconnecting</span>}
        {transport && <span className="sub">{plural(devices.length, 'device')}</span>}
        {transport && <AddDevice />}
        {transport && <ScanButton />}
      </header>

      {status.isError && <p className="banner error">The explorer is not answering: {status.error.message}</p>}

      {status.data && !transport ? (
        <AdapterPrompt
          adapters={status.data.adapters}
          busy={select.isPending ? select.variables : undefined}
          error={select.isError ? select.error.message : undefined}
          onPick={(a) => select.mutate(a)}
        />
      ) : (
        <main className="panes">
          <section className="tree-pane">
            <input
              className="filter"
              type="search"
              placeholder="Filter devices and objects"
              aria-label="Filter devices and objects"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
            {select.isError && <p className="banner error">{select.error.message}</p>}
            {rows.length === 0 ? (
              <p className="tree-empty sub">
                {devices.length === 0 ? 'No devices have answered yet.' : 'Nothing matches the filter.'}
              </p>
            ) : (
              <Tree rows={rows} selection={selection} behind={behind} onSelect={setSelection} onToggle={onToggle} />
            )}
          </section>
          <section className="detail-pane">
            {!selection || !selectedDevice ? (
              <EmptyDetail devices={devices.length} />
            ) : selection.kind === 'device' ? (
              <DeviceDetail
                key={selectedDevice.id}
                device={selectedDevice}
                router={
                  selectedDevice.network !== 0
                    ? devices.find((d) => d.network === 0 && d.address === selectedDevice.address)
                    : undefined
                }
                objects={objects.get(selectedDevice.id)}
              />
            ) : (
              <ObjectDetail
                key={`${selection.device}/${selection.type}/${selection.instance}`}
                device={selectedDevice}
                type={selection.type}
                instance={selection.instance}
                object={objects
                  .get(selection.device)
                  ?.objects.find((o) => o.type === selection.type && o.instance === selection.instance)}
              />
            )}
          </section>
        </main>
      )}
    </div>
  )
}

function ScanButton() {
  const scan = useMutation({ mutationFn: api.scan })
  const [sent, setSent] = useState(false)
  useEffect(() => {
    if (!scan.isSuccess) return
    setSent(true)
    const timer = setTimeout(() => setSent(false), 1500)
    return () => clearTimeout(timer)
  }, [scan.isSuccess, scan.submittedAt])
  return (
    <button
      className="button primary"
      onClick={() => scan.mutate()}
      disabled={scan.isPending}
      title={scan.isError ? scan.error.message : 'Broadcast a Who-Is and add the devices that answer'}
    >
      {scan.isError ? 'Scan failed' : sent ? 'Who-Is sent' : 'Scan'}
    </button>
  )
}

// AddDevice takes the address of a device that no Who-Is reaches, such as
// one on another IP subnet, and has the explorer read it instead.
function AddDevice() {
  const [open, setOpen] = useState(false)
  const [address, setAddress] = useState('')
  const add = useMutation({
    mutationFn: api.addDevice,
    onSuccess: () => {
      setAddress('')
      setOpen(false)
    },
  })
  if (!open) {
    return (
      <button className="button" onClick={() => setOpen(true)} title="List a device that does not answer the Who-Is, by its IP address">
        Add by address
      </button>
    )
  }
  return (
    <form
      className="add-device"
      onSubmit={(e) => {
        e.preventDefault()
        if (address.trim()) add.mutate(address.trim())
      }}
    >
      <input
        className="filter"
        type="text"
        placeholder="IP address, or IP:port"
        aria-label="Device IP address"
        value={address}
        autoFocus
        disabled={add.isPending}
        onChange={(e) => setAddress(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Escape') setOpen(false)
        }}
      />
      <button className="button primary" type="submit" disabled={add.isPending || !address.trim()}>
        {add.isPending ? 'Reading…' : 'Add'}
      </button>
      <button className="button" type="button" onClick={() => setOpen(false)} disabled={add.isPending}>
        Cancel
      </button>
      {add.isError && <span className="flag fault" title={add.error.message}>{add.error.message}</span>}
    </form>
  )
}

function TransportPicker({ status, busy, onPick }: { status: Status; busy: boolean; onPick: (adapter: string) => void }) {
  const transport = status.transport!
  if (status.adapters.length === 0) {
    return (
      <span className="transport">
        {transport.name} <span className="mono sub">{transport.local}</span>
      </span>
    )
  }
  return (
    <label className="transport">
      <span className="sub">Adapter</span>
      <select value={transport.name} disabled={busy} onChange={(e) => onPick(e.target.value)}>
        {!status.adapters.some((a) => a.name === transport.name) && <option>{transport.name}</option>}
        {status.adapters.map((a) => (
          <option key={`${a.name}/${a.address}`} value={a.name}>
            {a.name} · {a.address}
          </option>
        ))}
      </select>
      <span className="mono sub">{transport.local}</span>
    </label>
  )
}

interface AdapterPromptProps {
  adapters: Adapter[]
  busy?: string
  error?: string
  onPick: (adapter: string) => void
}

function AdapterPrompt({ adapters, busy, error, onPick }: AdapterPromptProps) {
  return (
    <main className="prompt">
      <h2>Which network?</h2>
      <p>
        The explorer broadcasts a Who-Is on the adapter you pick and lists the devices that answer. It reads from
        devices and never writes to them.
      </p>
      {error && <p className="banner error">{error}</p>}
      {adapters.length === 0 && <p className="banner error">No network adapter with an IPv4 address is up.</p>}
      <ul className="adapters">
        {adapters.map((a) => (
          <li key={`${a.name}/${a.address}`}>
            <button onClick={() => onPick(a.name)} disabled={busy !== undefined}>
              <strong>{a.name}</strong>
              <span className="mono">{a.address}</span>
              <span className="sub">{busy === a.name ? 'Opening…' : `Who-Is goes to ${a.broadcast}`}</span>
            </button>
          </li>
        ))}
      </ul>
    </main>
  )
}
