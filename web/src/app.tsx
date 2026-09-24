import { t, lang, setLang } from './i18n'
import { adminURL, navigate, route } from './router'
import { currentUser, logout, serverStatus, setupActive, toast } from './store'
import { AdminView } from './views/admin'
import { FilesView } from './views/files'
import { LoginView } from './views/login'
import { RecycleView } from './views/recycle'
import { SetupView } from './views/setup'

// App 是应用壳：先看要不要走向导，再决定登录页还是主界面。
export function App() {
  const status = serverStatus.value
  const user = currentUser.value
  const r = route.value

  if (status === null) {
    return <div class="boot">{t('loading')}</div>
  }
  // 系统里还没有用户 → 强制走首启向导（不管 hash 指向哪里）；
  // 向导进行到一半时（管理员刚建好）也要继续留在向导里。
  if (status.bootstrap_needed || setupActive.value) {
    return <SetupView />
  }
  if (!user) {
    return <LoginView />
  }

  const adminTabs: Array<[string, string]> = [
    ['shares', t('navShares')],
    ['users', t('navUsers')],
    ['pools', t('navPools')],
    ['tokens', t('navTokens')],
  ]

  return (
    <div class="shell">
      <header class="topbar">
        <a class="brand" href="#/files">
          {t('appName')}
        </a>
        <nav>
          <a class={r.view === 'files' ? 'active' : ''} href="#/files">
            {t('navFiles')}
          </a>
          {user.role === 'admin' &&
            adminTabs.map(([tab, label]) => (
              <a
                key={tab}
                class={r.view === 'admin' && r.tab === tab ? 'active' : ''}
                href={adminURL(tab)}
              >
                {label}
              </a>
            ))}
        </nav>
        <div class="right">
          <select
            class="lang"
            value={lang.value}
            onChange={(e) => setLang((e.currentTarget.value as 'en' | 'zh'))}
            title={t('navLanguage')}
          >
            <option value="en">English</option>
            <option value="zh">中文</option>
          </select>
          <span class="who">{user.username}</span>
          <button
            class="btn-secondary"
            onClick={() => {
              logout()
              navigate('#/login')
            }}
          >
            {t('navLogout')}
          </button>
        </div>
      </header>

      <main>
        {r.view === 'files' && <FilesView shareId={r.shareId} dirPath={r.path} />}
        {r.view === 'recycle' && <RecycleView shareId={r.shareId} />}
        {r.view === 'admin' && <AdminView tab={r.tab ?? 'shares'} />}
      </main>

      {toast.value && <div class="toast">{toast.value}</div>}
    </div>
  )
}
