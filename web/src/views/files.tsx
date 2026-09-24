import { useEffect, useRef, useState } from 'preact/hooks'
import {
  api,
  downloadURL,
  uploadBlob,
  type DirListView,
  type FileView,
  type UsageView,
} from '../api'
import { t } from '../i18n'
import { filesURL, navigate, recycleURL } from '../router'
import { isShareLocked } from '../share'
import { msg, notify, refreshShares, shares } from '../store'
import { HistoryModal } from '../history'
import { Empty, ErrorBox, Field, Modal, fmtBytes, fmtTime } from '../ui'

/** UploadState 描述"正在上传哪个文件、传了多少字节"。 */
interface UploadState {
  name: string
  /** 第几个文件 / 一共几个（多选上传时用） */
  index: number
  count: number
  loaded: number
  /** 0 表示总大小未知 */
  total: number
}

/** join 把目录与文件名拼成绝对路径。 */
function join(dir: string, name: string): string {
  const base = dir.endsWith('/') ? dir.slice(0, -1) : dir
  return `${base}/${name}`
}

// FilesView 是文件管理：左侧/顶部选共享，中间是目录列表，带上传、下载、
// 新建目录、改名、删除（进回收站）以及加密共享的解密/取消解密。
export function FilesView({ shareId, dirPath }: { shareId?: number; dirPath: string }) {
  const list = shares.value
  const activeId = shareId ?? list[0]?.id
  const share = list.find((s) => s.id === activeId)
  // 只有加密且未解锁的共享才不能写；未加密的共享 unlocked 同样是 false，
  // 直接用 unlocked 判断会把普通共享整个锁死（上传/建目录按钮永远灰着）。
  const locked = isShareLocked(share)
  const fileInput = useRef<HTMLInputElement>(null)

  const [entries, setEntries] = useState<FileView[]>([])
  const [usage, setUsage] = useState<UsageView | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [busy, setBusy] = useState('')
  const [upload, setUpload] = useState<UploadState | null>(null)
  const [historyOf, setHistoryOf] = useState<FileView | null>(null)
  const [dragging, setDragging] = useState(false)

  const [newFolder, setNewFolder] = useState<string | null>(null)
  const [renameTarget, setRenameTarget] = useState<FileView | null>(null)
  const [renameTo, setRenameTo] = useState('')
  const [unlockOpen, setUnlockOpen] = useState(false)
  const [unlockPw, setUnlockPw] = useState('')
  const [unlocking, setUnlocking] = useState(false)

  async function load() {
    if (!activeId) return
    setLoading(true)
    setError('')
    try {
      const q = new URLSearchParams({ path: dirPath })
      const dir = await api.get<DirListView>(`/api/shares/${activeId}/list?${q}`)
      setEntries(dir.entries ?? [])
      try {
        setUsage(await api.get<UsageView>(`/api/shares/${activeId}/usage`))
      } catch {
        setUsage(null) // 用量只是锦上添花，拿不到不影响浏览
      }
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
  }, [activeId, dirPath])

  async function uploadFiles(files: File[]) {
    if (!activeId || locked || files.length === 0) return
    let ok = 0
    for (const [i, f] of files.entries()) {
      setUpload({ name: f.name, index: i + 1, count: files.length, loaded: 0, total: f.size })
      setBusy(t('uploading', { name: f.name }))
      try {
        // 上传走 XHR（uploadBlob）而不是 fetch：只有它能汇报上传进度
        const { promise } = uploadBlob(
          `/api/shares/${activeId}/files?path=${encodeURIComponent(join(dirPath, f.name))}`,
          f,
          (loaded, total) => setUpload((prev) => (prev ? { ...prev, loaded, total } : prev)),
        )
        await promise
        ok++
      } catch (err) {
        notify(`${f.name}: ${msg(err)}`)
      }
    }
    setUpload(null)
    setBusy('')
    if (ok > 0) notify(t('uploadDone', { n: ok }))
    await load()
  }

  async function createFolder() {
    if (!activeId || !newFolder) return
    try {
      await api.post(`/api/shares/${activeId}/folders`, { path: join(dirPath, newFolder) })
      setNewFolder(null)
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function doRename() {
    if (!activeId || !renameTarget || !renameTo) return
    try {
      await api.post(`/api/shares/${activeId}/rename`, {
        from: renameTarget.path,
        to: join(dirPath, renameTo),
      })
      setRenameTarget(null)
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function remove(entry: FileView) {
    if (!activeId) return
    if (!window.confirm(t('deleteConfirm', { name: entry.name }))) return
    try {
      await api.del(`/api/shares/${activeId}/files?path=${encodeURIComponent(entry.path)}`)
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function unlockShare() {
    if (!activeId) return
    setUnlocking(true)
    try {
      await api.post(`/api/shares/${activeId}/unlock`, { password: unlockPw })
      setUnlockOpen(false)
      setUnlockPw('')
      await refreshShares()
      await load()
    } catch (err) {
      setError(msg(err))
    } finally {
      setUnlocking(false)
    }
  }

  async function lockShare() {
    if (!activeId) return
    try {
      await api.post(`/api/shares/${activeId}/lock`)
      await refreshShares()
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

  const crumbs = dirPath.split('/').filter((s) => s.length > 0)

  return (
    <div class="page">
      <div class="toolbar">
        <select
          class="share-pick"
          value={String(activeId ?? '')}
          onChange={(e) => navigate(filesURL(Number(e.currentTarget.value), '/'))}
        >
          {list.map((s) => (
            <option key={s.id} value={s.id}>
              {s.name}
              {s.encrypted ? (s.unlocked ? ' 🔓' : ' 🔒') : ''}
            </option>
          ))}
        </select>

        <button class="primary" disabled={locked} onClick={() => fileInput.current?.click()}>
          {t('upload')}
        </button>
        <input
          ref={fileInput}
          type="file"
          multiple
          class="file-input"
          onChange={(e) => {
            const files = Array.from(e.currentTarget.files ?? [])
            e.currentTarget.value = ''
            void uploadFiles(files)
          }}
        />
        <button disabled={locked} onClick={() => setNewFolder('')}>
          {t('newFolder')}
        </button>
        <button class="btn-secondary" onClick={() => activeId && navigate(recycleURL(activeId))}>
          {t('navRecycle')}
        </button>

        {share?.encrypted && (
          <button
            class={share.unlocked ? 'link' : 'primary'}
            onClick={() => (share.unlocked ? void lockShare() : setUnlockOpen(true))}
          >
            {share.unlocked ? t('lockShare') : t('unlockShare')}
          </button>
        )}
        <span class="spacer" />
        <button class="btn-secondary" onClick={() => void load()}>
          {t('refresh')}
        </button>
      </div>

      {usage && (
        <div class="usage">
          <div class="bar">
            <div
              class="fill"
              style={{
                width: `${usage.total_bytes > 0 ? Math.min(100, (usage.used_bytes / usage.total_bytes) * 100) : 0}%`,
              }}
            />
          </div>
          <span class="muted small">
            {t('usedOfTotal', { used: fmtBytes(usage.used_bytes), total: fmtBytes(usage.total_bytes) })}
          </span>
          <span class="muted small">
            {t('usageBreakdown', {
              current: fmtBytes(usage.current_bytes),
              history: fmtBytes(usage.history_bytes),
              recycle: fmtBytes(usage.recycle_bytes),
            })}
          </span>
          {usage.reclaimable_bytes > 0 && (
            <span class="muted small">
              {t('reclaimable', { size: fmtBytes(usage.reclaimable_bytes) })}
            </span>
          )}
        </div>
      )}

      <nav class="crumbs">
        <a href={activeId ? filesURL(activeId, '/') : '#/files'}>{t('root')}</a>
        {crumbs.map((seg, i) => (
          <span key={seg + i}>
            {' / '}
            <a href={activeId ? filesURL(activeId, '/' + crumbs.slice(0, i + 1).join('/')) : '#'}>
              {seg}
            </a>
          </span>
        ))}
      </nav>

      <ErrorBox text={error} />
      {upload && (
        <div class="upload-progress">
          <div class="upload-progress-head">
            <span class="upload-progress-name" title={upload.name}>
              {upload.name}
            </span>
            <span class="muted small">
              {upload.count > 1 ? `${upload.index}/${upload.count} · ` : ''}
              {upload.total > 0 && upload.loaded >= upload.total ? (
                // 字节传完了，但服务端还要编码 + 落盘，这段时间别让进度条看起来像卡住
                <>{t('uploadProcessing')}</>
              ) : upload.total > 0 ? (
                `${fmtBytes(upload.loaded)} / ${fmtBytes(upload.total)}`
              ) : (
                fmtBytes(upload.loaded)
              )}
            </span>
          </div>
          <div class="upload-progress-bar">
            <div
              class="upload-progress-fill"
              style={{
                width:
                  upload.total > 0
                    ? `${Math.min(100, (upload.loaded / upload.total) * 100)}%`
                    : '0%',
              }}
            />
          </div>
        </div>
      )}
      {busy && !upload && <p class="muted small">{busy}</p>}

      <div
        class={dragging && !locked ? 'dropzone dragging' : 'dropzone'}
        onDragOver={(e) => {
          e.preventDefault()
          setDragging(true)
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={(e) => {
          e.preventDefault()
          setDragging(false)
          if (locked) return
          const files = Array.from(e.dataTransfer?.files ?? [])
          void uploadFiles(files)
        }}
      >
        {loading && entries.length === 0 ? (
          <p class="muted">{t('loading')}</p>
        ) : entries.length === 0 ? (
          <Empty text={t('emptyFolder')} />
        ) : (
          <table class="grid">
            <thead>
              <tr>
                <th>{t('name')}</th>
                <th>{t('size')}</th>
                <th>{t('modified')}</th>
                <th>{t('actions')}</th>
              </tr>
            </thead>
            <tbody>
              {entries
                .slice()
                .sort((a, b) => (a.kind === b.kind ? a.name.localeCompare(b.name) : a.kind === 'dir' ? -1 : 1))
                .map((entry) => (
                  <tr key={entry.path}>
                    <td>
                      {entry.kind === 'dir' && activeId ? (
                        <a href={filesURL(activeId, entry.path)}>
                          📁 {entry.name}
                        </a>
                      ) : (
                        <span>📄 {entry.name}</span>
                      )}
                    </td>
                    <td>{entry.kind === 'dir' ? '-' : fmtBytes(entry.size)}</td>
                    <td>{fmtTime(entry.mtime)}</td>
                    <td class="actions">
                      {entry.kind !== 'dir' && activeId && (
                        <a class="btn-secondary" href={downloadURL(activeId, entry.path)} download={entry.name}>
                          {t('download')}
                        </a>
                      )}
                      <button class="btn-secondary" onClick={() => setHistoryOf(entry)}>
                        {t('history')}
                      </button>
                      <button
                        class="btn-secondary"
                        onClick={() => {
                          setRenameTarget(entry)
                          setRenameTo(entry.name)
                        }}
                      >
                        {t('renameItem')}
                      </button>
                      <button class="btn-secondary danger" onClick={() => void remove(entry)}>
                        {t('deleteItem')}
                      </button>
                    </td>
                  </tr>
                ))}
            </tbody>
          </table>
        )}
      </div>

      {historyOf && activeId && (
        <HistoryModal shareId={activeId} entry={historyOf} onClose={() => setHistoryOf(null)} />
      )}

      {newFolder !== null && (
        <Modal title={t('newFolder')} onClose={() => setNewFolder(null)}>
          <Field label={t('folderName')}>
            <input
              autofocus
              value={newFolder}
              onInput={(e) => setNewFolder(e.currentTarget.value)}
              onKeyDown={(e) => e.key === 'Enter' && void createFolder()}
            />
          </Field>
          <button disabled={!newFolder} onClick={() => void createFolder()}>
            {t('create')}
          </button>
        </Modal>
      )}

      {renameTarget && (
        <Modal title={t('renameItem')} onClose={() => setRenameTarget(null)}>
          <Field label={t('newName')}>
            <input
              autofocus
              value={renameTo}
              onInput={(e) => setRenameTo(e.currentTarget.value)}
              onKeyDown={(e) => e.key === 'Enter' && void doRename()}
            />
          </Field>
          <button disabled={!renameTo} onClick={() => void doRename()}>
            {t('save')}
          </button>
        </Modal>
      )}

      {unlockOpen && (
        <Modal title={t('unlockShare')} onClose={() => setUnlockOpen(false)}>
          <Field label={t('sharePassword')}>
            <input
              autofocus
              type="password"
              value={unlockPw}
              onInput={(e) => setUnlockPw(e.currentTarget.value)}
              onKeyDown={(e) => e.key === 'Enter' && void unlockShare()}
            />
          </Field>
          <button disabled={unlocking || !unlockPw} onClick={() => void unlockShare()}>
            {unlocking ? t('loading') : t('unlockShare')}
          </button>
        </Modal>
      )}
    </div>
  )
}
