import { signal } from '@preact/signals'

// 极简 hash 路由：前端挂在服务端的根路径上，用 #/… 区分页面，
// 这样刷新任何页面都只需要服务端回 index.html（见 internal/webui）。

export interface Route {
  view: 'setup' | 'login' | 'files' | 'recycle' | 'admin'
  /** admin 的子页：users / shares / pools / tokens */
  tab?: string
  shareId?: number
  /** 文件路径（以 / 开头） */
  path: string
}

function parse(hash: string): Route {
  const raw = hash.replace(/^#\/?/, '')
  const parts = raw.split('/').filter((p) => p.length > 0)
  const head = parts[0] ?? ''
  const rest = parts.slice(1)

  switch (head) {
    case 'login':
      return { view: 'login', path: '/' }
    case 'setup':
      return { view: 'setup', path: '/' }
    case 'files': {
      const shareId = rest.length > 0 ? Number(rest[0]) : undefined
      return {
        view: 'files',
        shareId: Number.isFinite(shareId) ? shareId : undefined,
        path: rest.length > 1 ? '/' + rest.slice(1).join('/') : '/',
      }
    }
    case 'recycle': {
      const shareId = rest.length > 0 ? Number(rest[0]) : undefined
      return { view: 'recycle', shareId: Number.isFinite(shareId) ? shareId : undefined, path: '/' }
    }
    case 'admin':
      return { view: 'admin', tab: rest[0] ?? 'shares', path: '/' }
    default:
      return { view: 'files', path: '/' }
  }
}

export const route = signal<Route>(parse(location.hash))

/** navigate 跳转到一个 hash 路径。 */
export function navigate(to: string): void {
  if (location.hash === to) {
    route.value = parse(to)
    return
  }
  location.hash = to
}

/** startRouter 开始监听 hash 变化。 */
export function startRouter(): void {
  window.addEventListener('hashchange', () => {
    route.value = parse(location.hash)
  })
  route.value = parse(location.hash)
}

// ── 便于拼 URL 的小工具 ──

export function filesURL(shareId: number, path = '/'): string {
  const clean = path.replace(/^\/+|\/+$/g, '')
  return `#/files/${shareId}${clean ? '/' + clean : ''}`
}

export function recycleURL(shareId: number): string {
  return `#/recycle/${shareId}`
}

export function adminURL(tab: string): string {
  return `#/admin/${tab}`
}
