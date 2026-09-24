import { useState } from 'preact/hooks'
import { api, type LoginView } from '../api'
import { t } from '../i18n'
import { loadSession, msg, setSession } from '../store'
import { ErrorBox, Field, LangSwitch } from '../ui'

// LoginView 是登录页：用户名 + 密码 + 验证器里的六位码 → JWT。
export function LoginView() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function submit(e: Event) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const res = await api.post<LoginView>('/api/auth/login', {
        username,
        password,
        otp_code: code,
      })
      setSession(res.token, res.user)
      await loadSession()
      location.hash = '#/files'
    } catch (err) {
      setError(msg(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div class="center">
      <form class="card" onSubmit={submit}>
        <div class="card-head">
          <h1>{t('appName')}</h1>
          <LangSwitch />
        </div>
        <h2>{t('loginTitle')}</h2>
        <p class="muted">{t('loginSubtitle')}</p>

        <Field label={t('username')}>
          <input
            value={username}
            autofocus
            autocomplete="username"
            onInput={(e) => setUsername(e.currentTarget.value)}
          />
        </Field>
        <Field label={t('password')}>
          <input
            type="password"
            value={password}
            autocomplete="current-password"
            onInput={(e) => setPassword(e.currentTarget.value)}
          />
        </Field>
        <Field label={t('otpCode')}>
          <input
            value={code}
            inputMode="numeric"
            maxLength={6}
            autocomplete="one-time-code"
            placeholder="123456"
            onInput={(e) => setCode(e.currentTarget.value)}
          />
        </Field>

        <ErrorBox text={error} />
        <button type="submit" disabled={busy || !username || !password || code.length !== 6}>
          {busy ? t('loading') : t('signIn')}
        </button>
      </form>
    </div>
  )
}
