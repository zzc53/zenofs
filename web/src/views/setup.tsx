import { useState } from 'preact/hooks'
import {
  api,
  type CreatedTokenView,
  type DiskView,
  type LoginView,
  type PoolView,
  type ShareView,
} from '../api'
import { t } from '../i18n'
import { msg, notify, refreshShares, refreshStatus, setSession, setupActive } from '../store'
import { generateSecret, otpauthURI, totp } from '../totp'
import { ErrorBox, Field, LangSwitch, QrCode, copyText } from '../ui'

// SetupView 是首次运行的引导向导：创建管理员 → 建存储池 → 加磁盘 → 建共享。
//
// 之所以前端也参与：bootstrap 这一个入口是免认证的，建完管理员之后所有操作都要 JWT，
// 所以向导第 1 步结束时会用刚拿到的 OTP 密钥算一个码直接登录（省得用户手工再输一遍）。
export function SetupView() {
  setupActive.value = true

  const [step, setStep] = useState(0)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  // 第 1 步：管理员
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  // 进来就有一个密钥（含二维码）；点"换一个密钥"会重新生成并清掉已输入的验证码
  const [secret, setSecret] = useState(generateSecret)
  const [code, setCode] = useState('')

  // 第 2 步：存储池
  const [poolName, setPoolName] = useState('default')
  const [chunkKb, setChunkKb] = useState('1024')
  const [pool, setPool] = useState<PoolView | null>(null)

  // 第 3 步：磁盘
  const [diskPath, setDiskPath] = useState('')
  const [diskUsage, setDiskUsage] = useState<'data' | 'cache'>('data')
  const [asParity, setAsParity] = useState(false)
  const [disks, setDisks] = useState<DiskView[]>([])
  // 池里已经有条带盘（数据盘）之后，才谈得上"再加一个校验分片"
  const hasStriped = disks.some((d) => d.Type === 0)

  // 第 4 步：共享
  const [shareName, setShareName] = useState('files')
  const [quota, setQuota] = useState('0')
  const [recycleTtl, setRecycleTtl] = useState('0')
  const [versionKeep, setVersionKeep] = useState('0')
  const [sharePassword, setSharePassword] = useState('')

  const dataDisks = disks.filter((d) => d.Type === 0).length
  const cacheDisks = disks.filter((d) => d.Type === 1).length

  async function run(fn: () => Promise<void>) {
    setBusy(true)
    setError('')
    try {
      await fn()
    } catch (err) {
      setError(msg(err))
    } finally {
      setBusy(false)
    }
  }

  const createAdmin = () =>
    run(async () => {
      const created = await api.post<CreatedTokenView>('/api/auth/bootstrap', {
        username,
        password,
        // 密钥由前端生成并展示（含二维码），这里带着它和验证码一起提交，服务端会校验
        otp_secret: secret,
        otp_code: code,
      })
      const effective = created.otp_secret || secret
      const login = await api.post<LoginView>('/api/auth/login', {
        username,
        password,
        otp_code: await totp(effective),
      })
      setSession(login.token, login.user)
      setStep(1)
    })

  const createPool = () =>
    run(async () => {
      const created = await api.post<PoolView>('/api/pools', {
        name: poolName,
        chunk_size_kb: Number(chunkKb) || 1024,
      })
      setPool(created)
      setStep(2)
    })

  const addDisk = () =>
    run(async () => {
      if (!pool) return
      const created = await api.post<DiskView>(`/api/pools/${pool.Id}/disks`, {
        path: diskPath.trim(),
        type: diskUsage,
        add_parity: diskUsage === 'data' && asParity,
      })
      setDisks([...disks, created])
      setDiskPath('')
      setAsParity(false)
      // 数据/校验分片数是池上的计数（同一块盘在不同条带里可能是 data 也可能是
      // parity，由建条带时的 shuffle 决定），所以只能重新读池，不能从磁盘列表里数。
      setPool(await api.get<PoolView>(`/api/pools/${pool.Id}`))
      notify(t('diskAdded'))
    })

  const createShare = () =>
    run(async () => {
      if (!pool) return
      await api.post<ShareView>('/api/shares', {
        name: shareName,
        pool_id: pool.Id,
        quota_mb: Number(quota) || 0,
        recycle_ttl_hours: Number(recycleTtl) || 0,
        version_keep: Number(versionKeep) || 0,
        ...(sharePassword ? { password: sharePassword } : {}),
      })
      await refreshShares()
      await refreshStatus()
      setStep(4)
    })

  const done = () => {
    setupActive.value = false
    location.hash = '#/files'
  }

  const steps = [t('stepAdmin'), t('stepPool'), t('stepDisk'), t('stepShare')]

  return (
    <div class="center">
      <div class="card wide">
        <div class="card-head">
          <h1>{t('appName')}</h1>
          <LangSwitch />
        </div>
        <h2>{t('setupTitle')}</h2>
        <p class="muted">{t('setupIntro')}</p>

        {step < 4 && (
          <ol class="steps">
            {steps.map((label, i) => (
              <li key={label} class={i === step ? 'current' : i < step ? 'done' : ''}>
                <span class="dot">{i + 1}</span>
                {label}
              </li>
            ))}
          </ol>
        )}
        {step < 4 && <p class="muted small">{t('step', { n: step + 1, total: 4 })}</p>}

        {step === 0 && (
          <>
            <Field label={t('username')}>
              <input value={username} onInput={(e) => setUsername(e.currentTarget.value)} />
            </Field>
            <Field label={t('password')} hint={t('hintMinPassword')}>
              <input
                type="password"
                value={password}
                onInput={(e) => setPassword(e.currentTarget.value)}
              />
            </Field>
            <div class="otp">
              <QrCode text={otpauthURI(secret, username || 'zenofs')} width={168} />
              <div class="otp-side">
                <p class="muted small">{t('otpScanHint')}</p>
                <code class="secret-text">{secret}</code>
                <div class="otp-actions">
                  <button class="btn-secondary" onClick={() => void copyText(secret)}>
                    {t('copy')}
                  </button>
                  <button
                    class="btn-secondary"
                    onClick={() => {
                      setSecret(generateSecret())
                      setCode('') // 换了密钥，之前那个码就作废了
                    }}
                  >
                    {t('otpRefresh')}
                  </button>
                </div>
              </div>
            </div>
            <Field label={t('otpCode')}>
              <input
                value={code}
                inputMode="numeric"
                maxLength={6}
                placeholder="123456"
                onInput={(e) => setCode(e.currentTarget.value)}
              />
            </Field>
            <ErrorBox text={error} />
            <button
              disabled={busy || username === '' || password.length < 8}
              onClick={createAdmin}
            >
              {busy ? t('loading') : t('next')}
            </button>
          </>
        )}

        {step === 1 && (
          <>
            <Field label={t('poolName')}>
              <input value={poolName} onInput={(e) => setPoolName(e.currentTarget.value)} />
            </Field>
            <Field label={t('chunkSize')} hint={t('hintChunkSize')}>
              <input
                value={chunkKb}
                inputMode="numeric"
                onInput={(e) => setChunkKb(e.currentTarget.value)}
              />
            </Field>
            <ErrorBox text={error} />
            <button disabled={busy || poolName === ''} onClick={createPool}>
              {busy ? t('loading') : t('next')}
            </button>
          </>
        )}

        {step === 2 && (
          <>
            <p class="muted">
              {t('disksAdded', {
                n: disks.length,
                d: pool?.DataShards ?? 0,
                p: pool?.ParityShards ?? 0,
                c: cacheDisks,
              })}
            </p>
            <Field label={t('diskType')} hint={t('hintDiskUsage')}>
              <select
                value={diskUsage}
                onChange={(e) => {
                  setDiskUsage(e.currentTarget.value as 'data' | 'cache')
                  setAsParity(false)
                }}
              >
                <option value="data">{t('dataDisk')}</option>
                <option value="cache" disabled={!hasStriped}>
                  {t('cacheDisk')}
                </option>
              </select>
            </Field>
            {diskUsage === 'cache' && !hasStriped && (
              <p class="muted small">{t('hintNeedDataDisk')}</p>
            )}
            <Field label={t('diskPath')} hint="/var/lib/zenofs/disk0">
              <input value={diskPath} onInput={(e) => setDiskPath(e.currentTarget.value)} />
            </Field>
            {diskUsage === 'data' && hasStriped && (
              <>
                <label class="check">
                  <input
                    type="checkbox"
                    checked={asParity}
                    onChange={(e) => setAsParity(e.currentTarget.checked)}
                  />
                  {t('diskAsParity')}
                </label>
                <p class="muted small">{t('hintParityShard')}</p>
              </>
            )}
            <ErrorBox text={error} />
            <div class="row">
              <button disabled={busy || diskPath.trim() === ''} onClick={addDisk}>
                {t('addDisk')}
              </button>
              <button disabled={busy || dataDisks < 1} onClick={() => setStep(3)}>
                {t('next')}
              </button>
            </div>
            {disks.length > 0 && (
              <ul class="plain">
                {disks.map((d) => (
                  <li key={d.Id}>
                    {d.Path} · {d.Type === 1 ? t('diskAsParity') : t('dataDisk')}
                  </li>
                ))}
              </ul>
            )}
          </>
        )}

        {step === 3 && (
          <>
            <Field label={t('shareName')}>
              <input value={shareName} onInput={(e) => setShareName(e.currentTarget.value)} />
            </Field>
            <Field label={t('quotaMb')}>
              <input value={quota} inputMode="numeric" onInput={(e) => setQuota(e.currentTarget.value)} />
            </Field>
            <Field label={t('recycleTtlHours')}>
              <input
                value={recycleTtl}
                inputMode="numeric"
                onInput={(e) => setRecycleTtl(e.currentTarget.value)}
              />
            </Field>
            <Field label={t('versionKeep')}>
              <input
                value={versionKeep}
                inputMode="numeric"
                onInput={(e) => setVersionKeep(e.currentTarget.value)}
              />
            </Field>
            <Field label={t('encryptionPassword')} hint={t('hintMinPassword')}>
              <input
                type="password"
                value={sharePassword}
                onInput={(e) => setSharePassword(e.currentTarget.value)}
              />
            </Field>
            <ErrorBox text={error} />
            <button disabled={busy || shareName === ''} onClick={createShare}>
              {busy ? t('loading') : t('finish')}
            </button>
          </>
        )}

        {step === 4 && (
          <>
            <p class="ok">{t('setupDone')}</p>
            <button onClick={done}>{t('goToFiles')}</button>
          </>
        )}
      </div>
    </div>
  )
}
