import { signal } from '@preact/signals'
import { ApiError, api, getToken, setToken, type ShareView, type StatusView, type UserView } from './api'
import { t } from './i18n'

// 全局状态用 signals：读它的组件会自动重渲染。
// token 存在 localStorage（api.ts 管），这里只放需要展示/联动的数据。

export const serverStatus = signal<StatusView | null>(null)
export const currentUser = signal<UserView | null>(null)
export const shares = signal<ShareView[]>([])
export const toast = signal<string>('')

// setupActive 表示"首次运行向导正在进行"：向导第 1 步建好管理员之后，
// bootstrap_needed 会变成 false，但流程还要继续走 pool / disk / share，
// 所以用这个标记把向导留在屏幕上，直到用户点完成。
export const setupActive = signal(false)

/** msg 把任意错误转成可展示的文案。 */
export function msg(err: unknown): string {
  // ApiError 的 message 在构造时已经优先取了后端的 message，后端保证它非空
  // （没有 message 时退回 str_code），所以这里直接用就行。
  if (err instanceof ApiError) return err.message || t('unknownError')
  if (err instanceof Error) return err.message
  return String(err)
}

/** notify 弹一条短提示（3 秒后自动消失）。 */
export function notify(text: string): void {
  toast.value = text
  window.setTimeout(() => {
    if (toast.value === text) toast.value = ''
  }, 3000)
}

/** bootstrap 是启动流程：先问状态（要不要走首启向导），有 token 就恢复会话。 */
export async function bootstrap(): Promise<void> {
  try {
    serverStatus.value = await api.get<StatusView>('/api/status')
  } catch (err) {
    notify(msg(err))
    return
  }
  if (!getToken()) return
  await loadSession()
}

/** loadSession 用现有 token 拉当前用户与可见的共享。 */
export async function loadSession(): Promise<void> {
  try {
    currentUser.value = await api.get<UserView>('/api/auth/me')
    await refreshShares()
  } catch (err) {
    if (err instanceof ApiError && err.isAuthError) logout()
    else notify(msg(err))
  }
}

/** refreshShares 重新拉共享列表。 */
export async function refreshShares(): Promise<void> {
  shares.value = await api.get<ShareView[]>('/api/shares')
}

/** setSession 保存登录态。 */
export function setSession(token: string, user: UserView): void {
  setToken(token)
  currentUser.value = user
}

/** logout 清掉本地登录态。 */
export function logout(): void {
  setToken('')
  currentUser.value = null
  shares.value = []
}

/** refreshStatus 重新问一次后端状态（向导完成后要用）。 */
export async function refreshStatus(): Promise<void> {
  serverStatus.value = await api.get<StatusView>('/api/status')
}

/** isAdmin 报告当前用户是不是管理员。 */
export function isAdmin(): boolean {
  return currentUser.value?.role === 'admin'
}
