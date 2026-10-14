import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { Admin } from './Admin'
import { api, type Field, type NodeID, type Source, type Values } from './api'
import { EmptyDetail, NodeDetail } from './Detail'
import { childrenQuery, useLive, useNodes } from './live'
import { plural, Tree } from './Tree'
import { buildRows, childrenOf } from './treeModel'

// The settings each source was opened with last time, so that the next
// visit starts the same way without asking.
const settingsKey = (kind: string) => `explorer.source.${kind}`

function remembered(kind: string): Values | null {
  try {
    const raw = localStorage.getItem(settingsKey(kind))
    return raw ? (JSON.parse(raw) as Values) : null
  } catch {
    return null
  }
}

function remember(kind: string, settings: Values) {
  try {
    localStorage.setItem(settingsKey(kind), JSON.stringify(settings))
  } catch {
    // Without storage the settings are asked for each time.
  }
}

export function App() {
  const qc = useQueryClient()
  const [selection, setSelection] = useState<NodeID | null>(null)
  const [toggled, setToggled] = useState<ReadonlyMap<string, boolean>>(new Map())
  const [filter, setFilter] = useState('')
  const [view, setView] = useState<'explore' | 'admin'>('explore')

  const connected = useLive(
    useCallback(() => {
      setSelection(null)
      setToggled(new Map())
    }, []),
  )
  const sources = useQuery({ queryKey: ['sources'], queryFn: api.sources })
  const nodes = useNodes()

  const open = useMutation({
    mutationFn: ({ kind, settings }: { kind: string; settings: Values }) => api.openSource(kind, settings),
    onSuccess: (list, { kind, settings }) => {
      remember(kind, settings)
      qc.setQueryData(['sources'], list)
    },
  })

  // Go back to the settings used last time, once, if nothing is open.
  const resumed = useRef(false)
  useEffect(() => {
    if (resumed.current || !sources.data) return
    resumed.current = true
    if (sources.data.some((s) => s.status.open)) return
    for (const s of sources.data) {
      const last = remembered(s.kind)
      if (last && s.settings.every((f) => !f.required || last[f.key])) {
        open.mutate({ kind: s.kind, settings: last })
        return
      }
    }
  }, [sources.data, open])

  // A node's children are read while its row is open.
  const openIDs = useMemo(
    () => [...nodes.values()].filter((n) => n.children.state !== 'none' && toggled.get(n.id)).map((n) => n.id),
    [nodes, toggled],
  )
  useQueries({ queries: openIDs.map((id) => childrenQuery(qc, id)) })

  const rows = useMemo(() => buildRows({ nodes, toggled, filter }), [nodes, toggled, filter])
  const children = useMemo(() => childrenOf(nodes), [nodes])

  const onToggle = useCallback((key: string, open: boolean) => {
    setToggled((prev) => new Map(prev).set(key, open))
  }, [])

  const opened = sources.data?.filter((s) => s.status.open) ?? []
  const anyOpen = opened.length > 0
  const selected = selection ? nodes.get(selection) : undefined
  const roots = children.get('')?.length ?? 0

  return (
    <div className="app">
      <header className="bar">
        <h1>
          <svg viewBox="0 0 16 16" aria-hidden>
            <rect width="16" height="16" rx="3" fill="currentColor" />
            <path d="M4 4h3v3H4zM9 9h3v3H9zM5.5 7v3.5H9" fill="none" stroke="var(--bar-bg)" strokeWidth="1.4" />
          </svg>
          Explorer
        </h1>
        {opened.map((s) => (
          <SourceBadge key={s.kind} source={s} />
        ))}
        <span className="pill" title="The explorer only reads. It never writes to a device.">
          read-only
        </span>
        <span className="spacer" />
        {!connected && <span className="flag fault">reconnecting</span>}
        {anyOpen && <span className="sub">{plural(roots, 'device')}</span>}
        <button
          className={`button${view === 'admin' ? ' primary' : ''}`}
          onClick={() => setView(view === 'admin' ? 'explore' : 'admin')}
          title="Decide which points the data consumers get"
        >
          Admin
        </button>
        {opened.map((s) => s.addForm.length > 0 && <AddNode key={`add-${s.kind}`} source={s} />)}
        {opened.map((s) => s.canDiscover && <DiscoverButton key={`scan-${s.kind}`} source={s} />)}
      </header>

      {sources.isError && <p className="banner error">The explorer is not answering: {sources.error.message}</p>}

      {view === 'admin' ? (
        <Admin nodes={nodes} onClose={() => setView('explore')} />
      ) : sources.data && !anyOpen ? (
        <SourcePrompt
          sources={sources.data}
          busy={open.isPending ? open.variables.kind : undefined}
          error={open.isError ? open.error.message : undefined}
          onOpen={(kind, settings) => open.mutate({ kind, settings })}
        />
      ) : (
        <main className="panes">
          <section className="tree-pane">
            <input
              className="filter"
              type="search"
              placeholder="Filter"
              aria-label="Filter devices and points"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
            {open.isError && <p className="banner error">{open.error.message}</p>}
            {rows.length === 0 ? (
              <p className="tree-empty sub">{nodes.size === 0 ? 'Nothing has answered yet.' : 'Nothing matches the filter.'}</p>
            ) : (
              <Tree rows={rows} selection={selection} onSelect={setSelection} onToggle={onToggle} />
            )}
          </section>
          <section className="detail-pane">
            {!selected ? (
              <EmptyDetail nodes={nodes.size} discovering={opened.some((s) => s.canDiscover)} />
            ) : (
              <NodeDetail key={selected.id} node={selected} under={children.get(selected.id) ?? []} />
            )}
          </section>
        </main>
      )}
    </div>
  )
}

function SourceBadge({ source }: { source: Source }) {
  const lines = [...(source.status.lines ?? []), ...(source.status.background ?? [])]
  return (
    <span className="transport" title={lines.map((l) => `${l.label}: ${l.value}`).join('\n')}>
      {source.status.summary ?? source.title}
    </span>
  )
}

function DiscoverButton({ source }: { source: Source }) {
  const scan = useMutation({ mutationFn: () => api.discover(source.kind) })
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
      title={scan.isError ? scan.error.message : source.discoverHint}
    >
      {scan.isError ? 'Scan failed' : sent ? 'Sent' : 'Scan'}
    </button>
  )
}

// FieldInput draws one field of a source's form.
function FieldInput({ field, value, onChange, disabled }: { field: Field; value: string; onChange: (v: string) => void; disabled?: boolean }) {
  const id = `field-${field.key}`
  return (
    <label className="field" htmlFor={id}>
      <span className="field-label">{field.label}</span>
      {field.type === 'select' ? (
        <select id={id} className="filter" value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
          {!field.required && <option value="">—</option>}
          {(field.options ?? []).map((o) => (
            <option key={o.value} value={o.value} title={o.hint}>
              {o.label}
              {o.hint ? ` · ${o.hint}` : ''}
            </option>
          ))}
        </select>
      ) : (
        <input
          id={id}
          className="filter"
          type={field.type === 'number' ? 'number' : 'text'}
          value={value}
          placeholder={field.hint}
          title={field.hint}
          disabled={disabled}
          required={field.required}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </label>
  )
}

function defaults(fields: Field[], last?: Values | null): Values {
  const v: Values = {}
  for (const f of fields) v[f.key] = last?.[f.key] ?? f.default ?? (f.type === 'select' && f.required ? (f.options?.[0]?.value ?? '') : '')
  return v
}

// AddNode takes the source's add form, for a device no Scan reaches.
function AddNode({ source }: { source: Source }) {
  const [open, setOpen] = useState(false)
  const [more, setMore] = useState(false)
  const [values, setValues] = useState<Values>(() => defaults(source.addForm))
  const add = useMutation({
    mutationFn: (v: Values) => api.addNode(source.kind, v),
    onSuccess: () => {
      setValues(defaults(source.addForm))
      setOpen(false)
    },
  })
  const complete = source.addForm.every((f) => !f.required || values[f.key]?.trim())
  if (!open) {
    return (
      <button className="button" onClick={() => setOpen(true)} title="List a device that does not answer the scan, by its address">
        Add by address
      </button>
    )
  }
  // The first field is shown; the rest unfold on request.
  const [first, ...rest] = source.addForm
  return (
    <form
      className="add-device"
      onSubmit={(e) => {
        e.preventDefault()
        if (complete) add.mutate(values)
      }}
    >
      <input
        className="filter"
        type="text"
        placeholder={first.hint ?? first.label}
        aria-label={first.label}
        value={values[first.key] ?? ''}
        autoFocus
        disabled={add.isPending}
        onChange={(e) => setValues({ ...values, [first.key]: e.target.value })}
        onKeyDown={(e) => {
          if (e.key === 'Escape') setOpen(false)
        }}
      />
      {more &&
        rest.map((f) => (
          <input
            key={f.key}
            className="filter"
            type={f.type === 'number' ? 'number' : 'text'}
            placeholder={f.label}
            title={f.hint}
            aria-label={f.label}
            value={values[f.key] ?? ''}
            disabled={add.isPending}
            onChange={(e) => setValues({ ...values, [f.key]: e.target.value })}
          />
        ))}
      {rest.length > 0 && !more && (
        <button className="link" type="button" onClick={() => setMore(true)}>
          more…
        </button>
      )}
      <button className="button primary" type="submit" disabled={add.isPending || !complete}>
        {add.isPending ? 'Reading…' : 'Add'}
      </button>
      <button className="button" type="button" onClick={() => setOpen(false)} disabled={add.isPending}>
        Cancel
      </button>
      {add.isError && (
        <span className="flag fault" title={add.error.message}>
          {add.error.message}
        </span>
      )}
    </form>
  )
}

interface SourcePromptProps {
  sources: Source[]
  busy?: string
  error?: string
  onOpen: (kind: string, settings: Values) => void
}

// SourcePrompt asks for a source's settings when nothing is open.
function SourcePrompt({ sources, busy, error, onOpen }: SourcePromptProps) {
  return (
    <main className="prompt">
      {sources.map((s) => (
        <SourceForm key={s.kind} source={s} busy={busy === s.kind} error={error} onOpen={onOpen} />
      ))}
      {sources.length === 0 && <p className="banner error">No source is available.</p>}
    </main>
  )
}

function SourceForm({ source, busy, error, onOpen }: { source: Source; busy: boolean; error?: string; onOpen: SourcePromptProps['onOpen'] }) {
  const [values, setValues] = useState<Values>(() => defaults(source.settings, remembered(source.kind)))
  const complete = source.settings.every((f) => !f.required || values[f.key])
  const adapter = source.settings.find((f) => f.type === 'select' && f.required)
  const noChoice = adapter && (adapter.options?.length ?? 0) === 0
  return (
    <form
      className="source-form"
      onSubmit={(e) => {
        e.preventDefault()
        if (complete) onOpen(source.kind, values)
      }}
    >
      <h2>{source.title}</h2>
      {source.blurb && <p>{source.blurb}</p>}
      {error && <p className="banner error">{error}</p>}
      {noChoice && <p className="banner error">Nothing to choose from: no {adapter.label.toLowerCase()} is available.</p>}
      {source.settings.map((f) => (
        <FieldInput key={f.key} field={f} value={values[f.key] ?? ''} disabled={busy} onChange={(v) => setValues({ ...values, [f.key]: v })} />
      ))}
      <button className="button primary" type="submit" disabled={busy || !complete}>
        {busy ? 'Opening…' : 'Open'}
      </button>
    </form>
  )
}
