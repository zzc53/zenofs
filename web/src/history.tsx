import { useEffect, useState } from 'preact/hooks'
import { api, type FileView, type HistoryView, type VersionView } from './api'
import { t } from './i18n'
import { canWrite } from './share'
import { msg, notify, shares } from './store'
import { ErrorBox, Modal, fmtBytes, fmtTime } from './ui'

/** eventLabel 把事件名翻成文案。用显式映射而不是拼 key，才能保住类型检查。 */
function eventLabel(event: HistoryView['event']): string {
  switch (event) {
    case 'created':
      return t('eventCreated')
    case 'renamed':
      return t('eventRenamed')
    case 'moved':
      return t('eventMoved')
    case 'deleted':
      return t('eventDeleted')
    case 'restored':
      return t('eventRestored')
    default:
      return t('eventUnknown')
  }
}

/** detail 给出该事件的细节：改名显示新旧名，移动显示去向。 */
function detail(h: HistoryView): string {
  switch (h.event) {
    case 'renamed':
      return `${h.old_name ?? ''} → ${h.new_name ?? ''}`
    case 'moved':
      return h.new_parent ? `${t('moveTo')} ${h.new_parent}` : ''
    default:
      return ''
  }
}

/**
 * HistoryModal 展示一个条目的版本与变更记录，并允许把它恢复到某个版本。
 *
 * 版本和变更都按 inode id 查（不是路径），所以**回收站里的条目也能用同一个弹窗**：
 * 它已经没有可访问的路径了，但 inode 还在。
 *
 * 恢复需要写权限：没有写权限时按钮直接禁用，免得点了才吃 403。
 */
export function HistoryModal(props: { shareId: number; entry: FileView; onClose: () => void }) {
  const [tab, setTab] = useState<'versions' | 'changes'>('versions')
  const [versions, setVersions] = useState<VersionView[]>([])
  const [changes, setChanges] = useState<HistoryView[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const writable = canWrite(shares.value.find((s) => s.id === props.shareId))

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function load() {
    setError('')
    const base = `/api/shares/${props.shareId}/inodes/${props.entry.id}`
    try {
      setVersions((await api.get<VersionView[]>(`${base}/versions`)) ?? [])
      setChanges((await api.get<HistoryView[]>(`${base}/history`)) ?? [])
    } catch (err) {
      setError(msg(err))
    }
  }

  async function restore(v: VersionView) {
    if (!window.confirm(t('restoreVersionConfirm', { name: props.entry.name }))) return
    setBusy(true)
    setError('')
    try {
      await api.post(`/api/shares/${props.shareId}/versions/${v.id}/restore`)
      notify(t('versionRestored'))
      await load()
    } catch (err) {
      setError(msg(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title={`${props.entry.name} · ${t('history')}`} onClose={props.onClose}>
      <div class="tabs">
        <button
          class={tab === 'versions' ? 'tab active' : 'tab'}
          onClick={() => setTab('versions')}
        >
          {t('tabVersions')} ({versions.length})
        </button>
        <button class={tab === 'changes' ? 'tab active' : 'tab'} onClick={() => setTab('changes')}>
          {t('tabChanges')} ({changes.length})
        </button>
      </div>

      <ErrorBox text={error} />

      {tab === 'versions' &&
        (versions.length === 0 ? (
          <p class="muted">{t('versionsEmpty')}</p>
        ) : (
          <table class="grid">
            <tbody>
              {versions.map((v) => (
                <tr key={v.id}>
                  <td>{fmtTime(v.created_at)}</td>
                  <td>{fmtBytes(v.size)}</td>
                  <td class="muted small">{t('byUser', { id: v.created_by })}</td>
                  <td>
                    {v.is_current && <span class="badge">{t('versionCurrent')}</span>}
                  </td>
                  <td class="actions">
                    {!v.is_current && (
                      <button
                        class="btn-secondary"
                        disabled={busy || !writable}
                        onClick={() => void restore(v)}
                      >
                        {t('restoreVersion')}
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ))}

      {tab === 'changes' &&
        (changes.length === 0 ? (
          <p class="muted">{t('changesEmpty')}</p>
        ) : (
          <table class="grid">
            <tbody>
              {changes.map((h) => (
                <tr key={h.id}>
                  <td>{fmtTime(h.created_at)}</td>
                  <td>{eventLabel(h.event)}</td>
                  <td class="muted small">{detail(h)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ))}
    </Modal>
  )
}
