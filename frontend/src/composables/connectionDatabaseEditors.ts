import { getCurrentScope, inject, onScopeDispose, reactive, watch, type Ref } from 'vue'
import { listConnectionCatalogs, updateConnectionDatabase, type Connection } from '@/api/connections'
import { supportsDatabaseEditing } from '@/utils/connectionDatabase'

export const CONNECTION_DATABASE_EDITORS_KEY = 'connection-database-editors'

export interface ConnectionDatabaseEditor {
  readonly connectionId: number
  savedDatabase: string
  draft: string
  readonly dirty: boolean
  saving: boolean
  loadingCatalogs: boolean
  catalogsLoaded: boolean
  catalogs: string[]
  error: string
  catalogError: string
  setDraft(value: string): void
  resetDraft(): void
  loadCatalogs(force?: boolean): Promise<void>
  save(): Promise<Connection | undefined>
}

type SavedListener = (connection: Connection) => void

export interface ConnectionDatabaseEditors {
  editorFor(connection: Connection): ConnectionDatabaseEditor
  isBlocked(id: number | null | undefined): boolean
  isLocked(id: number | null | undefined): boolean
  onSaved(listener: SavedListener): () => void
  acquireLock(ids: (number | null | undefined)[]): () => void
  dispose(): void
}

function connectionVersion(connection: Connection): string {
  return JSON.stringify([
    connection.id, connection.db_type, connection.host, connection.port,
    connection.database, connection.username, connection.name, connection.env,
  ])
}

function errorMessage(error: unknown, fallback: string): string {
  const response = (error as { response?: { data?: { error?: string; message?: string } } })?.response
  return response?.data?.error || response?.data?.message || fallback
}

/** One migration workspace owns this store, so all tabs and both ends share drafts. */
export function createConnectionDatabaseEditors(
  connections: Ref<Connection[]>,
  onSaved?: SavedListener,
  externallyLocked: (id: number) => boolean = () => false,
): ConnectionDatabaseEditors {
  const editors = reactive(new Map<number, {
    editor: ConnectionDatabaseEditor
    sync(connection?: Connection): void
    invalidate(): void
  }>())
  const locks = reactive(new Map<number, number>())
  const listeners = new Set<SavedListener>(onSaved ? [onSaved] : [])
  let disposed = false

  function isLocked(id: number | null | undefined): boolean {
    return id != null && (externallyLocked(id) || (locks.get(id) ?? 0) > 0)
  }

  function editorFor(initial: Connection): ConnectionDatabaseEditor {
    const existing = editors.get(initial.id)
    if (existing) return existing.editor
    let connection = { ...(connections.value.find(c => c.id === initial.id) ?? initial) }
    let version = connectionVersion(connection)
    let generation = 0
    let catalogRequest = 0
    let available = true

    function sync(next?: Connection) {
      if (!next) {
        available = false
        generation++
        catalogRequest++
        editor.saving = false
        editor.loadingCatalogs = false
        editor.error = '该连接已删除，请重新选择连接'
        return
      }
      const nextVersion = connectionVersion(next)
      connection = { ...next }
      available = true
      if (version === nextVersion) return
      version = nextVersion
      generation++
      catalogRequest++
      editor.savedDatabase = next.database
      editor.draft = next.database
      editor.saving = false
      editor.loadingCatalogs = false
      editor.catalogsLoaded = false
      editor.catalogs = []
      editor.error = ''
      editor.catalogError = ''
    }

    const editor: ConnectionDatabaseEditor = reactive({
      connectionId: connection.id,
      savedDatabase: connection.database,
      draft: connection.database,
      get dirty() { return editor.draft !== editor.savedDatabase },
      saving: false,
      loadingCatalogs: false,
      catalogsLoaded: false,
      catalogs: [] as string[],
      error: '',
      catalogError: '',
      setDraft(value: string) {
        if (disposed || editor.saving || isLocked(connection.id)) return
        editor.draft = value
        editor.error = ''
      },
      resetDraft() {
        editor.setDraft(editor.savedDatabase)
      },
      async loadCatalogs(force = false) {
        if (disposed || !available || !supportsDatabaseEditing(connection.db_type)) return
        if (!force && (editor.catalogsLoaded || editor.loadingCatalogs)) return
        const request = ++catalogRequest
        const currentGeneration = generation
        editor.loadingCatalogs = true
        editor.catalogError = ''
        try {
          const { data } = await listConnectionCatalogs(connection.id)
          if (disposed || request !== catalogRequest || generation !== currentGeneration) return
          editor.catalogs = [...new Set(data)]
          editor.catalogsLoaded = true
        } catch (error) {
          if (disposed || request !== catalogRequest || generation !== currentGeneration) return
          editor.catalogs = []
          editor.catalogError = errorMessage(error, '无法获取数据库列表')
        } finally {
          if (!disposed && request === catalogRequest && generation === currentGeneration) editor.loadingCatalogs = false
        }
      },
      async save() {
        if (disposed || !available || editor.saving || isLocked(connection.id) || !supportsDatabaseEditing(connection.db_type)) return
        if (!editor.dirty) return
        if (!editor.draft.trim()) {
          editor.error = '请输入数据库名称'
          return
        }
        const currentGeneration = generation
        editor.saving = true
        editor.error = ''
        let saved: Connection
        try {
          const { data } = await updateConnectionDatabase(connection.id, editor.draft, editor.savedDatabase)
          if (disposed || generation !== currentGeneration) return
          saved = data
        } catch (error) {
          if (!disposed && generation === currentGeneration) editor.error = errorMessage(error, '连接验证失败，数据库名称未保存')
          return
        } finally {
          if (!disposed && generation === currentGeneration) editor.saving = false
        }
        sync(saved)
        connections.value = connections.value.map(c => c.id === saved.id ? saved : c)
        for (const listener of listeners) listener(saved)
        return saved
      },
    })
    editors.set(initial.id, {
      editor,
      sync,
      invalidate() { generation++; catalogRequest++ },
    })
    return editor
  }

  const stop = watch(connections, current => {
    for (const [id, entry] of editors) entry.sync(current.find(c => c.id === id))
  }, { deep: true, flush: 'sync' })

  function dispose() {
    disposed = true
    stop()
    for (const entry of editors.values()) entry.invalidate()
    listeners.clear()
    locks.clear()
  }
  if (getCurrentScope()) onScopeDispose(dispose)

  return {
    editorFor,
    isBlocked(id) {
      if (id == null) return false
      const editor = editors.get(id)?.editor
      return Boolean(editor && (editor.dirty || editor.saving))
    },
    isLocked,
    onSaved(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    acquireLock(ids) {
      const validIds = [...new Set(ids.filter((id): id is number => id != null))]
      for (const id of validIds) locks.set(id, (locks.get(id) ?? 0) + 1)
      let released = false
      return () => {
        if (released) return
        released = true
        for (const id of validIds) {
          const count = (locks.get(id) ?? 1) - 1
          if (count > 0) locks.set(id, count)
          else locks.delete(id)
        }
      }
    },
    dispose,
  }
}

export function useConnectionDatabaseEditors(): ConnectionDatabaseEditors {
  const editors = inject<ConnectionDatabaseEditors | null>(CONNECTION_DATABASE_EDITORS_KEY, null)
  if (!editors) throw new Error('DatabaseNameEditor requires the migration workspace database editor provider')
  return editors
}
