import { useEffect, useState } from 'preact/hooks'
import {
  api,
  type CreatedTokenView,
  type CreatedUserView,
  type DiskView,
  type GrantView,
  type PoolView,
  type ShareView,
  type TaskView,
  type TokenView,
  type UsageView,
  type UserView,
} from '../api'
import { t } from '../i18n'
import { currentUser, msg, notify, refreshShares } from '../store'
import { generateSecret, otpauthURI } from '../totp'
import { Empty, ErrorBox, Field, Modal, QrCode, copyText, fmtBytes, fmtTime } from '../ui'

// AdminView 把四个管理页放在一起：共享 / 用户 / 存储池 / 访问凭证。
export function AdminView({ tab }: { tab: string }) {
  switch (tab) {
    case 'users':
      return <UsersAdmin />
    case 'pools':
      return <PoolsAdmin />
    case 'tokens':
      return <TokensAdmin />
    default:
      return <SharesAdmin />
  }
}

// ─────────────────────────────────────────────────────────────
// 共享
// ─────────────────────────────────────────────────────────────

function SharesAdmin() {
  const [list, setList] = useState<ShareView[]>([])
  const [pools, setPools] = useState<PoolView[]>([])
  const [usage, setUsage] = useState<Record<number, UsageView>>({})
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const [grantFor, setGrantFor] = useState<ShareView | null>(null)
  const [form, setForm] = useState({
    name: '',
    pool_id: '',
    quota_mb: '0',
    compression: '0',
    password: '',
  })

  async function load() {
    setError('')
    try {
      const [shares, poolList] = await Promise.all([
        api.get<ShareView[]>('/api/shares'),
        api.get<PoolView[]>('/api/pools'),
      ])
      setList(shares)
      setPools(poolList)
      const collected: Record<number, UsageView> = {}
      await Promise.all(
        shares.map(async (s) => {
          try {
            collected[s.id] = await api.get<UsageView>(`/api/shares/${s.id}/usage`)
          } catch {
            // 没授权的共享取不到用量，忽略
          }
        }),
      )
      setUsage(collected)
    } catch (err) {
      setError(msg(err))
    }
  }

  useEffect(() => {
    void load()
  }, [])

  async function create() {
    setError('')
    try {
      await api.post('/api/shares', {
        name: form.name,
        pool_id: Number(form.pool_id),
        quota_mb: Number(form.quota_mb) || 0,
        compression: Number(form.compression) || 0,
        ...(form.password ? { password: form.password } : {}),
      })
      setCreating(false)
      setForm({ ...form, name: '', password: '' })
      await load()
      await refreshShares()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function remove(share: ShareView, force = false) {
    setError('')
    try {
      await api.del(`/api/shares/${share.id}${force ? '?force=1' : ''}`)
      await load()
      await refreshShares()
    } catch (err) {
      if (err instanceof Error && 'status' in err && (err as { status: number }).status === 400) {
        if (window.confirm(`${t('shareNotEmpty')}\n${t('forceDelete')}?`)) {
          await remove(share, true)
          return
        }
      }
      setError(msg(err))
    }
  }

  async function unlock(share: ShareView) {
    const pw = window.prompt(t('sharePassword'))
    if (!pw) return
    try {
      await api.post(`/api/shares/${share.id}/unlock`, { password: pw })
      await load()
      await refreshShares()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function lock(share: ShareView) {
    try {
      await api.post(`/api/shares/${share.id}/lock`)
      await load()
      await refreshShares()
    } catch (err) {
      setError(msg(err))
    }
  }

  return (
    <div class="page">
      <div class="toolbar">
        <h2>{t('sharesTitle')}</h2>
        <span class="spacer" />
        <button onClick={() => setCreating(true)}>{t('newShare')}</button>
        <button class="btn-secondary" onClick={() => void load()}>
          {t('refresh')}
        </button>
      </div>

      <ErrorBox text={error} />

      {list.length === 0 ? (
        <Empty text={t('noShares')} />
      ) : (
        <table class="grid">
          <thead>
            <tr>
              <th>{t('name')}</th>
              <th>{t('pool')}</th>
              <th>{t('usage')}</th>
              <th>{t('permission')}</th>
              <th>{t('lockState')}</th>
              <th>{t('actions')}</th>
            </tr>
          </thead>
          <tbody>
            {list.map((s) => (
              <tr key={s.id}>
                <td>{s.name}</td>
                <td>{s.pool_id}</td>
                <td>
                  {usage[s.id]
                    ? `${fmtBytes(usage[s.id].used_bytes)} / ${fmtBytes(usage[s.id].total_bytes)}`
                    : '-'}
                </td>
                <td>{s.permission ?? '-'}</td>
                <td>
                  {s.encrypted ? (s.unlocked ? t('stateUnlocked') : t('stateLocked')) : t('none')}
                </td>
                <td class="actions">
                  {s.encrypted && (
                    <button class="btn-secondary" onClick={() => (s.unlocked ? void lock(s) : void unlock(s))}>
                      {s.unlocked ? t('lockShare') : t('unlockShare')}
                    </button>
                  )}
                  <button class="btn-secondary" onClick={() => setGrantFor(s)}>
                    {t('grantedUsers')}
                  </button>
                  <button class="btn-secondary danger" onClick={() => void remove(s)}>
                    {t('delete')}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {creating && (
        <Modal
          title={t('newShare')}
          onClose={() => setCreating(false)}
          footer={
            <button disabled={!form.name || !form.pool_id} onClick={() => void create()}>
              {t('create')}
            </button>
          }
        >
          <Field label={t('shareName')}>
            <input value={form.name} onInput={(e) => setForm({ ...form, name: e.currentTarget.value })} />
          </Field>
          <Field label={t('pool')}>
            <select value={form.pool_id} onChange={(e) => setForm({ ...form, pool_id: e.currentTarget.value })}>
              <option value="">—</option>
              {pools.map((p) => (
                <option key={p.Id} value={p.Id}>
                  {p.Name} (#{p.Id})
                </option>
              ))}
            </select>
          </Field>
          <Field label={t('quotaMb')}>
            <input
              value={form.quota_mb}
              inputMode="numeric"
              onInput={(e) => setForm({ ...form, quota_mb: e.currentTarget.value })}
            />
          </Field>
          <Field label={t('compression')}>
            <select
              value={form.compression}
              onChange={(e) => setForm({ ...form, compression: e.currentTarget.value })}
            >
              <option value="0">{t('compressionNone')}</option>
              <option value="1">{t('compressionZstd')}</option>
            </select>
          </Field>
          <Field label={t('encryptionPassword')} hint={t('hintMinPassword')}>
            <input
              type="password"
              value={form.password}
              onInput={(e) => setForm({ ...form, password: e.currentTarget.value })}
            />
          </Field>
        </Modal>
      )}

      {grantFor && (
        <GrantModal
          share={grantFor}
          onClose={() => setGrantFor(null)}
          onChanged={() => {
            void load()
            void refreshShares()
          }}
        />
      )}
    </div>
  )
}

function GrantModal(props: { share: ShareView; onClose: () => void; onChanged: () => void }) {
  const [grants, setGrants] = useState<GrantView[]>([])
  const [users, setUsers] = useState<UserView[]>([])
  const [userId, setUserId] = useState('')
  const [permission, setPermission] = useState('write')
  const [error, setError] = useState('')

  async function load() {
    setError('')
    try {
      const [gs, us] = await Promise.all([
        api.get<GrantView[]>(`/api/shares/${props.share.id}/users`),
        api.get<UserView[]>('/api/users'),
      ])
      setGrants(gs ?? [])
      setUsers(us)
    } catch (err) {
      setError(msg(err))
    }
  }
  useEffect(() => {
    void load()
  }, [])

  async function grant() {
    setError('')
    try {
      await api.post(`/api/shares/${props.share.id}/users`, {
        user_id: Number(userId),
        permission,
      })
      setUserId('')
      await load()
      props.onChanged()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function revoke(g: GrantView) {
    try {
      await api.del(`/api/shares/${props.share.id}/users/${g.user_id}`)
      await load()
      props.onChanged()
    } catch (err) {
      setError(msg(err))
    }
  }

  return (
    <Modal title={`${t('grantedUsers')} · ${props.share.name}`} onClose={props.onClose}>
      <ErrorBox text={error} />
      {grants.length === 0 ? (
        <Empty text={t('none')} />
      ) : (
        <ul class="plain">
          {grants.map((g) => (
            <li key={g.user_id}>
              {g.username ?? `#${g.user_id}`} · {g.permission}
              <button class="btn-secondary danger" onClick={() => void revoke(g)}>
                {t('revoke')}
              </button>
            </li>
          ))}
        </ul>
      )}
      <div class="row">
        <select value={userId} onChange={(e) => setUserId(e.currentTarget.value)}>
          <option value="">{t('selectUser')}</option>
          {users.map((u) => (
            <option key={u.id} value={u.id}>
              {u.username}
            </option>
          ))}
        </select>
        <select value={permission} onChange={(e) => setPermission(e.currentTarget.value)}>
          <option value="read">{t('permRead')}</option>
          <option value="write">{t('permWrite')}</option>
          <option value="admin">{t('permAdmin')}</option>
        </select>
        <button disabled={!userId} onClick={() => void grant()}>
          {t('grant')}
        </button>
      </div>
    </Modal>
  )
}

// ─────────────────────────────────────────────────────────────
// 用户
// ─────────────────────────────────────────────────────────────

function UsersAdmin() {
  const [users, setUsers] = useState<UserView[]>([])
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<UserView | null>(null)
  const [secret, setSecret] = useState<{ name: string; secret: string; uri: string } | null>(null)
  const [form, setForm] = useState({
    username: '',
    password: '',
    role: 'user',
    otp_secret: generateSecret(), // 默认就有一个，配二维码显示
    otp_code: '',
  })
  const [edit, setEdit] = useState({ password: '', role: '', reset_otp: false, otp_secret: '', otp_code: '' })

  async function load() {
    setError('')
    try {
      setUsers(await api.get<UserView[]>('/api/users'))
    } catch (err) {
      setError(msg(err))
    }
  }
  useEffect(() => {
    void load()
  }, [])

  async function create() {
    setError('')
    try {
      const created = await api.post<CreatedUserView>('/api/users', {
        username: form.username,
        password: form.password,
        role: form.role,
        otp_secret: form.otp_secret,
        otp_code: form.otp_code,
      })
      if (created.otp_secret) {
        setSecret({ name: created.username, secret: created.otp_secret, uri: created.otp_uri ?? '' })
      }
      setCreating(false)
      setForm({ username: '', password: '', role: 'user', otp_secret: generateSecret(), otp_code: '' })
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function saveEdit() {
    if (!editing) return
    setError('')
    try {
      const updated = await api.put<CreatedUserView>(`/api/users/${editing.id}`, {
        ...(edit.password ? { password: edit.password } : {}),
        ...(edit.role ? { role: edit.role } : {}),
        ...(edit.reset_otp ? { reset_otp: true, otp_secret: edit.otp_secret, otp_code: edit.otp_code } : {}),
      })
      if (updated.otp_secret) {
        setSecret({ name: updated.username, secret: updated.otp_secret, uri: updated.otp_uri ?? '' })
      } else {
        notify(t('passwordChanged'))
      }
      setEditing(null)
      setEdit({ password: '', role: '', reset_otp: false, otp_secret: '', otp_code: '' })
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function remove(user: UserView) {
    if (!window.confirm(t('deleteUserConfirm', { name: user.username }))) return
    try {
      await api.del(`/api/users/${user.id}`)
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  const me = currentUser.value

  return (
    <div class="page">
      <div class="toolbar">
        <h2>{t('usersTitle')}</h2>
        <span class="spacer" />
        <button onClick={() => setCreating(true)}>{t('newUser')}</button>
        <button class="btn-secondary" onClick={() => void load()}>
          {t('refresh')}
        </button>
      </div>

      <ErrorBox text={error} />

      <table class="grid">
        <thead>
          <tr>
            <th>{t('username')}</th>
            <th>{t('role')}</th>
            <th>{t('otpCode')}</th>
            <th>{t('createdAt')}</th>
            <th>{t('actions')}</th>
          </tr>
        </thead>
        <tbody>
          {users.map((u) => (
            <tr key={u.id}>
              <td>{u.username}</td>
              <td>{u.role === 'admin' ? t('roleAdmin') : t('roleUser')}</td>
              <td>{u.otp_enabled ? '✓' : '-'}</td>
              <td>{fmtTime(u.created_at)}</td>
              <td class="actions">
                <button
                  class="btn-secondary"
                  onClick={() => {
                    setEditing(u)
                    setEdit({ password: '', role: u.role, reset_otp: false, otp_secret: '', otp_code: '' })
                  }}
                >
                  {t('editUser')}
                </button>
                <button class="btn-secondary danger" disabled={u.id === me?.id} onClick={() => void remove(u)}>
                  {t('delete')}
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {creating && (
        <Modal
          title={t('newUser')}
          onClose={() => setCreating(false)}
          footer={
            <button disabled={!form.username || form.password.length < 8} onClick={() => void create()}>
              {t('create')}
            </button>
          }
        >
          <Field label={t('username')}>
            <input value={form.username} onInput={(e) => setForm({ ...form, username: e.currentTarget.value })} />
          </Field>
          <Field label={t('password')} hint={t('hintMinPassword')}>
            <input
              type="password"
              value={form.password}
              onInput={(e) => setForm({ ...form, password: e.currentTarget.value })}
            />
          </Field>
          <Field label={t('role')}>
            <select value={form.role} onChange={(e) => setForm({ ...form, role: e.currentTarget.value })}>
              <option value="user">{t('roleUser')}</option>
              <option value="admin">{t('roleAdmin')}</option>
            </select>
          </Field>
          <div class="otp">
            <QrCode text={otpauthURI(form.otp_secret, form.username || 'zenofs')} width={150} />
            <div class="otp-side">
              <p class="muted small">{t('otpScanHint')}</p>
              <code class="secret-text">{form.otp_secret}</code>
              <div class="otp-actions">
                <button class="btn-secondary" onClick={() => void copyText(form.otp_secret)}>
                  {t('copy')}
                </button>
                <button
                  class="btn-secondary"
                  onClick={() => setForm({ ...form, otp_secret: generateSecret(), otp_code: '' })}
                >
                  {t('otpRefresh')}
                </button>
              </div>
            </div>
          </div>
          <Field label={t('otpCode')}>
            <input
              value={form.otp_code}
              inputMode="numeric"
              maxLength={6}
              onInput={(e) => setForm({ ...form, otp_code: e.currentTarget.value })}
            />
          </Field>
        </Modal>
      )}

      {editing && (
        <Modal
          title={`${t('editUser')} · ${editing.username}`}
          onClose={() => setEditing(null)}
          footer={<button onClick={() => void saveEdit()}>{t('save')}</button>}
        >
          <Field label={t('newPassword')} hint={t('hintPasswordKeep')}>
            <input
              type="password"
              value={edit.password}
              onInput={(e) => setEdit({ ...edit, password: e.currentTarget.value })}
            />
          </Field>
          <Field label={t('role')}>
            <select value={edit.role} onChange={(e) => setEdit({ ...edit, role: e.currentTarget.value })}>
              <option value="user">{t('roleUser')}</option>
              <option value="admin">{t('roleAdmin')}</option>
            </select>
          </Field>
          <label class="check">
            <input
              type="checkbox"
              checked={edit.reset_otp}
              onChange={(e) => setEdit({ ...edit, reset_otp: e.currentTarget.checked })}
            />
            {t('resetOtp')}
          </label>
          {edit.reset_otp && (
            <>
              <Field label={t('otpSecret')} hint={t('resetOtpHint')}>
                <input
                  value={edit.otp_secret}
                  onInput={(e) => setEdit({ ...edit, otp_secret: e.currentTarget.value })}
                />
              </Field>
              <Field label={t('otpCode')}>
                <input
                  value={edit.otp_code}
                  inputMode="numeric"
                  maxLength={6}
                  onInput={(e) => setEdit({ ...edit, otp_code: e.currentTarget.value })}
                />
              </Field>
            </>
          )}
        </Modal>
      )}

      {secret && (
        <Modal title={t('secretReset')} onClose={() => setSecret(null)}>
          <p class="muted">{t('otpSecretGenerated')}</p>
          <code class="secret-text">{secret.secret}</code>
          <div class="otp-actions">
            <button class="btn-secondary" onClick={() => void copyText(secret.secret)}>
              {t('copy')}
            </button>
          </div>
          {secret.uri && <p class="muted small">{secret.uri}</p>}
          <p class="muted small">{secret.name}</p>
        </Modal>
      )}
    </div>
  )
}

// ─────────────────────────────────────────────────────────────
// 存储池与磁盘
// ─────────────────────────────────────────────────────────────

function PoolsAdmin() {
  const [pools, setPools] = useState<PoolView[]>([])
  const [disks, setDisks] = useState<Record<number, DiskView[]>>({})
  const [tasks, setTasks] = useState<TaskView[]>([])
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const [addingDisk, setAddingDisk] = useState<PoolView | null>(null)
  const [swapping, setSwapping] = useState<DiskView | null>(null)
  const [form, setForm] = useState({ name: '', chunk_size_kb: '1024' })
  const [diskForm, setDiskForm] = useState({
    path: '',
    usage: 'data' as 'data' | 'cache',
    add_parity: false,
  })
  const [swapPath, setSwapPath] = useState('')

  async function load() {
    setError('')
    try {
      const [poolList, taskList] = await Promise.all([
        api.get<PoolView[]>('/api/pools'),
        api.get<TaskView[]>('/api/tasks?limit=20'),
      ])
      setPools(poolList)
      setTasks(taskList ?? [])
      const byPool: Record<number, DiskView[]> = {}
      await Promise.all(
        poolList.map(async (p) => {
          byPool[p.Id] = await api.get<DiskView[]>(`/api/pools/${p.Id}/disks`)
        }),
      )
      setDisks(byPool)
    } catch (err) {
      setError(msg(err))
    }
  }
  useEffect(() => {
    void load()
  }, [])

  async function createPool() {
    setError('')
    try {
      await api.post('/api/pools', { name: form.name, chunk_size_kb: Number(form.chunk_size_kb) || 1024 })
      setCreating(false)
      setForm({ name: '', chunk_size_kb: '1024' })
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function addDisk() {
    if (!addingDisk) return
    setError('')
    try {
      await api.post(`/api/pools/${addingDisk.Id}/disks`, {
        path: diskForm.path.trim(),
        type: diskForm.usage,
        add_parity: diskForm.usage === 'data' && diskForm.add_parity,
      })
      setAddingDisk(null)
      notify(t('diskAdded'))
      setDiskForm({ path: '', usage: 'data', add_parity: false })
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function deleteCacheDisk(disk: DiskView) {
    if (!window.confirm(t('deleteDiskConfirm', { path: disk.Path }))) return
    setError('')
    try {
      await api.del(`/api/disks/${disk.Id}`)
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function swapDisk() {
    if (!swapping) return
    setError('')
    try {
      await api.put(`/api/disks/${swapping.Id}/swap`, { path: swapPath.trim() })
      setSwapping(null)
      setSwapPath('')
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function offline(pool: PoolView) {
    if (!window.confirm(t('offlineConfirm', { name: pool.Name }))) return
    try {
      await api.put(`/api/pools/${pool.Id}/offline`)
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  async function rebuild(pool: PoolView) {
    try {
      const res = await api.post<{ queued: number }>(`/api/pools/${pool.Id}/reconstruct`)
      notify(t('rebuildQueued', { n: res.queued }))
      await load()
    } catch (err) {
      setError(msg(err))
    }
  }

  const statusName = (s: number) =>
    s === 0 ? t('online') : s === 1 ? t('offline') : t('repair')

  return (
    <div class="page">
      <div class="toolbar">
        <h2>{t('poolsTitle')}</h2>
        <span class="spacer" />
        <button onClick={() => setCreating(true)}>{t('newPool')}</button>
        <button class="btn-secondary" onClick={() => void load()}>
          {t('refresh')}
        </button>
      </div>

      <ErrorBox text={error} />

      {pools.length === 0 ? (
        <Empty text={t('none')} />
      ) : (
        pools.map((p) => (
          <section class="pool" key={p.Id}>
            <div class="pool-head">
              <strong>
                {p.Name} (#{p.Id})
              </strong>
              <span class="muted small">
                {t('dataShards')} {p.DataShards} · {t('parityShards')} {p.ParityShards} ·{' '}
                {p.ChunkSize} KB · {statusName(p.Status)}
              </span>
              <span class="spacer" />
              <button class="btn-secondary" onClick={() => setAddingDisk(p)}>
                {t('addDisk')}
              </button>
              <button class="btn-secondary" onClick={() => void rebuild(p)}>
                {t('rebuild')}
              </button>
              <button class="btn-secondary danger" onClick={() => void offline(p)}>
                {t('offline')}
              </button>
            </div>
            <table class="grid">
              <thead>
                <tr>
                  <th>{t('diskId')}</th>
                  <th>{t('path')}</th>
                  <th>{t('diskType')}</th>
                  <th>{t('status')}</th>
                  <th>{t('actions')}</th>
                </tr>
              </thead>
              <tbody>
                {(disks[p.Id] ?? []).map((d) => (
                  <tr key={d.Id}>
                    <td>{d.Id}</td>
                    <td>{d.Path}</td>
                    <td>{d.Type === 1 ? t('cacheDisk') : t('dataDisk')}</td>
                    <td>{statusName(d.Status)}</td>
                    <td class="actions">
                      {d.Type === 1 ? (
                        // 缓存盘上的数据只是副本：直接删掉即可，不需要"换盘 + 重建"
                        <button class="btn-secondary danger" onClick={() => void deleteCacheDisk(d)}>
                          {t('delete')}
                        </button>
                      ) : (
                        <button
                          class="btn-secondary"
                          onClick={() => {
                            setSwapping(d)
                            setSwapPath(d.Path)
                          }}
                        >
                          {t('swapDisk')}
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
        ))
      )}

      <h3>{t('tasks')}</h3>
      {tasks.length === 0 ? (
        <Empty text={t('noTasks')} />
      ) : (
        <table class="grid">
          <thead>
            <tr>
              <th>{t('taskName')}</th>
              <th>{t('status')}</th>
              <th>{t('taskMessage')}</th>
              <th>{t('modified')}</th>
            </tr>
          </thead>
          <tbody>
            {tasks.map((task) => (
              <tr key={task.id}>
                <td>{task.name}</td>
                <td>{task.status}</td>
                <td>{task.message}</td>
                <td>{fmtTime(task.updated_at || task.created_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {creating && (
        <Modal
          title={t('newPool')}
          onClose={() => setCreating(false)}
          footer={
            <button disabled={!form.name} onClick={() => void createPool()}>
              {t('create')}
            </button>
          }
        >
          <Field label={t('poolName')}>
            <input value={form.name} onInput={(e) => setForm({ ...form, name: e.currentTarget.value })} />
          </Field>
          <Field label={t('chunkSize')} hint={t('hintChunkSize')}>
            <input
              value={form.chunk_size_kb}
              inputMode="numeric"
              onInput={(e) => setForm({ ...form, chunk_size_kb: e.currentTarget.value })}
            />
          </Field>
        </Modal>
      )}

      {addingDisk && (
        <Modal
          title={`${t('addDisk')} · ${addingDisk.Name}`}
          onClose={() => setAddingDisk(null)}
          footer={
            <button disabled={!diskForm.path.trim()} onClick={() => void addDisk()}>
              {t('add')}
            </button>
          }
        >
          <Field label={t('diskType')} hint={t('hintDiskUsage')}>
            <select
              value={diskForm.usage}
              onChange={(e) =>
                setDiskForm({
                  ...diskForm,
                  usage: e.currentTarget.value as 'data' | 'cache',
                  add_parity: false,
                })
              }
            >
              <option value="data">{t('dataDisk')}</option>
              <option value="cache" disabled={addingDisk.DataShards === 0}>
                {t('cacheDisk')}
              </option>
            </select>
          </Field>
          {diskForm.usage === 'cache' && addingDisk.DataShards === 0 && (
            <p class="muted small">{t('hintNeedDataDisk')}</p>
          )}
          <Field label={t('diskPath')}>
            <input
              value={diskForm.path}
              onInput={(e) => setDiskForm({ ...diskForm, path: e.currentTarget.value })}
            />
          </Field>
          {diskForm.usage === 'data' && addingDisk.DataShards + addingDisk.ParityShards > 0 && (
            <>
              <label class="check">
                <input
                  type="checkbox"
                  checked={diskForm.add_parity}
                  onChange={(e) => setDiskForm({ ...diskForm, add_parity: e.currentTarget.checked })}
                />
                {t('diskAsParity')}
              </label>
              <p class="muted small">{t('hintParityShard')}</p>
            </>
          )}
        </Modal>
      )}

      {swapping && (
        <Modal
          title={t('swapDisk')}
          onClose={() => setSwapping(null)}
          footer={
            <button disabled={!swapPath.trim()} onClick={() => void swapDisk()}>
              {t('save')}
            </button>
          }
        >
          <Field label={t('swapTo')}>
            <input value={swapPath} onInput={(e) => setSwapPath(e.currentTarget.value)} />
          </Field>
        </Modal>
      )}
    </div>
  )
}

// ─────────────────────────────────────────────────────────────
// 访问凭证
// ─────────────────────────────────────────────────────────────

function TokensAdmin() {
  const [users, setUsers] = useState<UserView[]>([])
  const [userId, setUserId] = useState<number | null>(null)
  const [tokens, setTokens] = useState<TokenView[]>([])
  const [error, setError] = useState('')
  const [newToken, setNewToken] = useState<string>('')
  const [creating, setCreating] = useState(false)
  const [addingKey, setAddingKey] = useState(false)
  const [form, setForm] = useState({ name: '', expires_at: '' })
  const [keyForm, setKeyForm] = useState({ name: '', public_key: '' })

  async function loadUsers() {
    try {
      const us = await api.get<UserView[]>('/api/users')
      setUsers(us)
      if (us.length > 0 && userId === null) setUserId(currentUser.value?.id ?? us[0].id)
    } catch (err) {
      setError(msg(err))
    }
  }

  async function loadTokens(id: number) {
    setError('')
    try {
      setTokens((await api.get<TokenView[]>(`/api/users/${id}/tokens`)) ?? [])
    } catch (err) {
      setTokens([])
      setError(msg(err))
    }
  }

  useEffect(() => {
    void loadUsers()
  }, [])
  useEffect(() => {
    if (userId !== null) void loadTokens(userId)
  }, [userId])

  async function createToken() {
    if (userId === null) return
    setError('')
    try {
      const created = await api.post<CreatedTokenView>(`/api/users/${userId}/tokens`, {
        name: form.name,
        expires_at: form.expires_at ? Math.floor(new Date(form.expires_at).getTime() / 1000) : 0,
      })
      setNewToken(created.token ?? '')
      setCreating(false)
      setForm({ name: '', expires_at: '' })
      await loadTokens(userId)
    } catch (err) {
      setError(msg(err))
    }
  }

  async function addPubKey() {
    if (userId === null) return
    setError('')
    try {
      await api.post(`/api/users/${userId}/pubkeys`, {
        name: keyForm.name,
        public_key: keyForm.public_key,
      })
      setAddingKey(false)
      setKeyForm({ name: '', public_key: '' })
      await loadTokens(userId)
    } catch (err) {
      setError(msg(err))
    }
  }

  async function revoke(tok: TokenView) {
    if (!window.confirm(t('revokeConfirm'))) return
    try {
      await api.del(`/api/tokens/${tok.id}`)
      if (userId !== null) await loadTokens(userId)
    } catch (err) {
      setError(msg(err))
    }
  }

  const expireLabel = (v: number) => (v ? fmtTime(v) : t('never'))

  return (
    <div class="page">
      <div class="toolbar">
        <h2>{t('tokensTitle')}</h2>
        <select
          value={String(userId ?? '')}
          onChange={(e) => setUserId(Number(e.currentTarget.value))}
        >
          {users.map((u) => (
            <option key={u.id} value={u.id}>
              {u.username}
            </option>
          ))}
        </select>
        <span class="spacer" />
        <button onClick={() => setCreating(true)}>{t('newToken')}</button>
        <button onClick={() => setAddingKey(true)}>{t('addPubKey')}</button>
        <button class="btn-secondary" onClick={() => userId !== null && void loadTokens(userId)}>
          {t('refresh')}
        </button>
      </div>

      <p class="muted small">{t('tokensHint')}</p>
      <ErrorBox text={error} />

      {tokens.length === 0 ? (
        <Empty text={t('none')} />
      ) : (
        <table class="grid">
          <thead>
            <tr>
              <th>{t('name')}</th>
              <th>{t('kind')}</th>
              <th>{t('expiresAt')}</th>
              <th>{t('lastUsed')}</th>
              <th>{t('actions')}</th>
            </tr>
          </thead>
          <tbody>
            {tokens.map((tok) => (
              <tr key={tok.id}>
                <td>{tok.name || `#${tok.id}`}</td>
                <td>{tok.kind === 'public_key' ? t('kindPubKey') : t('kindToken')}</td>
                <td>{expireLabel(tok.expires_at)}</td>
                <td>{tok.last_used_at ? fmtTime(tok.last_used_at) : '-'}</td>
                <td class="actions">
                  {tok.kind === 'public_key' && <span class="muted small">{tok.fingerprint}</span>}
                  <button class="btn-secondary danger" onClick={() => void revoke(tok)}>
                    {t('revoke')}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {creating && (
        <Modal
          title={t('newToken')}
          onClose={() => setCreating(false)}
          footer={<button onClick={() => void createToken()}>{t('create')}</button>}
        >
          <Field label={t('tokenLabel')}>
            <input value={form.name} onInput={(e) => setForm({ ...form, name: e.currentTarget.value })} />
          </Field>
          <Field label={t('expiresAt')} hint={t('never')}>
            <input
              type="date"
              value={form.expires_at}
              onInput={(e) => setForm({ ...form, expires_at: e.currentTarget.value })}
            />
          </Field>
        </Modal>
      )}

      {addingKey && (
        <Modal
          title={t('addPubKey')}
          onClose={() => setAddingKey(false)}
          footer={
            <button disabled={!keyForm.public_key.trim()} onClick={() => void addPubKey()}>
              {t('add')}
            </button>
          }
        >
          <Field label={t('name')}>
            <input value={keyForm.name} onInput={(e) => setKeyForm({ ...keyForm, name: e.currentTarget.value })} />
          </Field>
          <Field label={t('publicKey')} hint="ssh-ed25519 AAAA… user@host">
            <textarea
              rows={3}
              value={keyForm.public_key}
              onInput={(e) => setKeyForm({ ...keyForm, public_key: e.currentTarget.value })}
            />
          </Field>
        </Modal>
      )}

      {newToken && (
        <Modal title={t('tokenValue')} onClose={() => setNewToken('')}>
          <p class="muted">{t('tokenShownOnce')}</p>
          <code class="secret-text">{newToken}</code>
          <div class="otp-actions">
            <button class="btn-secondary" onClick={() => void copyText(newToken)}>
              {t('copy')}
            </button>
          </div>
        </Modal>
      )}
    </div>
  )
}
