import { useEffect, useState } from 'preact/hooks'
import { api, type DeletedView } from '../api'
import { t } from '../i18n'
import { filesURL, navigate } from '../router'
import { msg, shares } from '../store'
import { Empty, ErrorBox, fmtBytes, fmtTime } from '../ui'

// RecycleView 是回收站：列出被软删除的条目，可以恢复或彻底删除。
export function RecycleView({ shareId }: { shareId?: number }) {
  const list = shares.value
  const activeId = shareId ?? list[0]?.id

  const [entries, setEntries] = useState<DeletedView[]>([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  async function load() {
    if (!activeId) return
    setLoading(true)
    setError('')
    try {
      setEntries((await api.get<DeletedView[]>(`/api/shares/${activeId}/recycle`)) ?? [])
    } catch (err) {
      setEntries([])
      setError(msg(err))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeId])

  async function restore(entry: DeletedView) {
    if (!activeId) return
    setError('')
    try {
      await api.post(`/api/shares/${activeId}/recycle/${entry.id}/restore`)
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function purge(entry: DeletedView) {
    if (!activeId) return
    if (!window.confirm(t('purgeConfirm', { name: entry.name }))) return
    try {
      await api.del(`/api/shares/${activeId}/recycle/${entry.id}`)
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function emptyTrash() {
    if (!activeId) return
    if (!window.confirm(t('emptyTrashConfirm'))) return
    try {
      await api.del(`/api/shares/${activeId}/recycle`)
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  if (list.length === 0) {
    return (
      <div class="page">
        <Empty text={t('noShares')} />
      </div>
    )
  }

  return (
    <div class="page">
      <div class="toolbar">
        <select
          class="share-pick"
          value={String(activeId ?? '')}
          onChange={(e) => navigate(`#/recycle/${Number(e.currentTarget.value)}`)}
        >
          {list.map((s) => (
            <option key={s.id} value={s.id}>
              {s.name}
            </option>
          ))}
        </select>
        <button class="link" onClick={() => activeId && navigate(filesURL(activeId, '/'))}>
          {t('navFiles')}
        </button>
        <span class="spacer" />
        <button class="link danger" disabled={entries.length === 0} onClick={() => void emptyTrash()}>
          {t('emptyTrash')}
        </button>
        <button class="link" onClick={() => void load()}>
          {t('refresh')}
        </button>
      </div>

      <ErrorBox text={error} />

      {loading && entries.length === 0 ? (
        <p class="muted">{t('loading')}</p>
      ) : entries.length === 0 ? (
        <Empty text={t('recycleEmpty')} />
      ) : (
        <table class="grid">
          <thead>
            <tr>
              <th>{t('name')}</th>
              <th>{t('size')}</th>
              <th>{t('deletedAt')}</th>
              <th>{t('deletedBy')}</th>
              <th>{t('actions')}</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry) => (
              <tr key={entry.id}>
                <td>
                  {entry.kind === 'dir' ? '📁' : '📄'} {entry.path}
                </td>
                <td>{entry.kind === 'dir' ? '-' : fmtBytes(entry.size)}</td>
                <td>{fmtTime(entry.deleted_at)}</td>
                <td>{entry.deleted_by || '-'}</td>
                <td class="actions">
                  <button class="link" onClick={() => void restore(entry)}>
                    {t('restore')}
                  </button>
                  <button class="link danger" onClick={() => void purge(entry)}>
                    {t('purge')}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
