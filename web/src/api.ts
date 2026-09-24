// API 客户端：统一拼路径、带 Bearer token、把后端的结构化错误转成 ApiError。
//
// 后端所有错误都是 {"code":<number>,"str_code":"...","message":"..."} 形式（见 internal/errs）。

export interface ApiErrorBody {
  code: number
  str_code: string
  message: string
  value?: string
}

export class ApiError extends Error {
  readonly status: number
  readonly code: number
  readonly strCode: string

  constructor(status: number, body: ApiErrorBody | null, fallback: string) {
    super(body?.message || fallback)
    this.name = 'ApiError'
    this.status = status
    this.code = body?.code ?? 0
    this.strCode = body?.str_code ?? ''
  }

  /** 未认证 / token 失效：前端据此跳回登录页。 */
  get isAuthError(): boolean {
    return this.status === 401
  }

  /** 资源还被锁着（加密 Share 未解锁）。 */
  get isLocked(): boolean {
    return this.status === 423
  }
}

const TOKEN_KEY = 'zenofs.token'
let token = localStorage.getItem(TOKEN_KEY) ?? ''

export function getToken(): string {
  return token
}

export function setToken(next: string): void {
  token = next
  if (next) localStorage.setItem(TOKEN_KEY, next)
  else localStorage.removeItem(TOKEN_KEY)
}

interface RequestOptions {
  /** 响应不是 JSON 时（比如下载原始字节）直接返回文本。 */
  raw?: boolean
}

async function request<T>(method: string, path: string, body?: unknown, opts: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = {}
  if (token) headers['Authorization'] = `Bearer ${token}`

  let payload: BodyInit | undefined
  if (body !== undefined) {
    if (body instanceof Blob || typeof body === 'string' || body instanceof FormData) {
      payload = body as BodyInit
    } else {
      headers['Content-Type'] = 'application/json'
      payload = JSON.stringify(body)
    }
  }

  let resp: Response
  try {
    resp = await fetch(path, { method, headers, body: payload })
  } catch (err) {
    throw new ApiError(0, null, err instanceof Error ? err.message : `${method} ${path} failed`)
  }

  if (!resp.ok) {
    let parsed: ApiErrorBody | null = null
    try {
      parsed = (await resp.json()) as ApiErrorBody
    } catch {
      // 非 JSON 响应（例如网关错误页），保留状态码即可
    }
    throw new ApiError(resp.status, parsed, `${method} ${path} failed (${resp.status})`)
  }

  if (resp.status === 204) return undefined as T
  const ctype = resp.headers.get('Content-Type') ?? ''
  if (opts.raw || !ctype.includes('application/json')) {
    return (await resp.text()) as unknown as T
  }
  return (await resp.json()) as T
}

export const api = {
  get: <T>(path: string) => request<T>('GET', path),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body),
  del: <T>(path: string) => request<T>('DELETE', path),
  /** 上传原始字节（文件内容就是 body）。不带进度，需要进度用 uploadBlob。 */
  putBlob: <T>(path: string, data: Blob) => request<T>('PUT', path, data),
}

/**
 * uploadBlob 上传原始字节，并通过 onProgress 汇报**上传**进度（字节）。
 *
 * 为什么不用 fetch：fetch 拿不到上传进度（用 ReadableStream 当请求体只有 Chromium 系
 * 支持，写法也绕）。XMLHttpRequest 的 upload.onprogress 是唯一在主流浏览器都稳的办法。
 *
 * 返回 abort 是留给"取消上传"用的：目前 UI 还没接，但接口先留好。
 */
export function uploadBlob(
  path: string,
  data: Blob,
  onProgress?: (loaded: number, total: number) => void,
): { promise: Promise<void>; abort: () => void } {
  const xhr = new XMLHttpRequest()

  const promise = new Promise<void>((resolve, reject) => {
    xhr.open('PUT', path)
    if (token) xhr.setRequestHeader('Authorization', `Bearer ${token}`)
    xhr.setRequestHeader('Content-Type', 'application/octet-stream')

    xhr.upload.onprogress = (e) => {
      // lengthComputable 为 false 时 total 不可信，交给调用方按"不确定"处理
      if (e.lengthComputable) onProgress?.(e.loaded, e.total)
    }
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve()
        return
      }
      let body: ApiErrorBody | null = null
      try {
        body = JSON.parse(xhr.responseText) as ApiErrorBody
      } catch {
        // 非 JSON 响应（例如网关错误页），保留状态码即可
      }
      reject(new ApiError(xhr.status, body, `PUT ${path} failed (${xhr.status})`))
    }
    xhr.onerror = () => reject(new ApiError(0, null, `PUT ${path} failed`))
    xhr.onabort = () => reject(new ApiError(0, null, `PUT ${path} aborted`))

    xhr.send(data)
  })

  return { promise, abort: () => xhr.abort() }
}

// downloadURL 拼一个能直接用 <a href> / window.open 打开的下载链接。
//
// 浏览器的原生下载发不出 Authorization 头，所以把 token 放在 query 里；
// 后端在最外层会先把它从 URL 摘掉再记访问日志（见 internal/api 的 stripAccessToken）。
export function downloadURL(shareId: number, filePath: string): string {
  const q = new URLSearchParams({ path: filePath, access_token: token })
  return `/api/shares/${shareId}/files?${q.toString()}`
}

// ─────────────────────────────────────────────────────────────
// 与后端约定的数据结构（字段名跟 JSON 对齐）
// ─────────────────────────────────────────────────────────────

export interface UserView {
  id: number
  username: string
  role: 'admin' | 'user'
  otp_enabled: boolean
  created_at: number
}

export interface StatusView {
  version: string
  bootstrap_needed: boolean
}

export interface LoginView {
  token: string
  token_type: string
  expires_in: number
  user: UserView
}

export interface ShareView {
  id: number
  name: string
  pool_id: number
  quota_mb: number
  compression: number
  encryption: number
  created_by: number
  created_at: number
  // 保留策略：0 表示不限
  recycle_ttl_hours: number
  version_keep: number
  encrypted: boolean
  unlocked: boolean
  permission?: string
}

export interface UsageView {
  share_id: number
  quota_mb: number
  total_bytes: number
  used_bytes: number
  free_bytes: number
  total_files: number
  free_files: number
  // 盘上真实占用（按分片去重统计），口径与 used_bytes 不同：
  // used_bytes 是"所有 inode 当前版本大小之和"（含回收站、不含历史版本），
  // current/history/recycle 才是这批数据在磁盘上的实际归属。
  current_bytes: number
  history_bytes: number
  recycle_bytes: number
  reclaimable_bytes: number
}

// PoolUsageView 是存储池的真实占用（分片在盘上的实际大小）。
export interface PoolUsageView {
  pool_id: number
  data_bytes: number
  parity_bytes: number
  used_slots: number
  free_slots: number
  // 谁都不引用的存量分片：可以直接回收（保护期内的不算）
  orphan_chunks: number
  orphan_bytes: number
}

export interface FileView {
  id: number
  name: string
  path: string
  kind: 'file' | 'dir' | 'link'
  size: number
  mode: number
  mtime: number
  target?: string
}

/** VersionView 是文件的一个版本（对应 vfs.VersionEntry）。 */
export interface VersionView {
  id: number
  size: number
  hash: string
  compression: number
  encryption: number
  created_by: number
  created_at: number
  is_current: boolean
}

/** HistoryView 是一条元数据变更记录（对应 vfs.HistoryEntry）。 */
export interface HistoryView {
  id: number
  event: 'created' | 'renamed' | 'moved' | 'deleted' | 'restored' | 'unknown'
  old_name?: string
  new_name?: string
  /** 父目录路径（不是 inode id）；父目录也没了就为空 */
  old_parent?: string
  new_parent?: string
  created_at: number
}

export interface DeletedView extends FileView {
  deleted_at: number
  deleted_by: number
}

export interface DirListView {
  path: string
  entries: FileView[]
}

export interface GrantView {
  user_id: number
  username?: string
  permission: string
}

// 存储池与磁盘端点直接序列化 Go 结构体，所以字段名是大写的。
export interface PoolView {
  Id: number
  Name: string
  DataShards: number
  ParityShards: number
  ChunkSize: number
  Status: number
}

export interface DiskView {
  Id: number
  Path: string
  PoolId: number
  Backend: number
  Type: number
  Status: number
}

export interface TaskView {
  id: number
  name: string
  status: string
  message: string
  metadata?: unknown
  created_at: number
  updated_at: number
}

export interface TokenView {
  id: number
  user_id: number
  name: string
  kind: 'secret' | 'public_key'
  fingerprint?: string
  public_key?: string
  expires_at: number
  created_at: number
  last_used_at: number
}

/** 创建 / 修改用户的响应：用户视图 + 服务端生成时一次性返回的 OTP 密钥。 */
export interface CreatedUserView extends UserView {
  otp_secret?: string
  otp_uri?: string
}

export interface CreatedTokenView extends TokenView {
  /** 只在创建 token 时返回一次。 */
  token?: string
  /** 服务端生成 OTP 密钥时返回一次。 */
  otp_secret?: string
  otp_uri?: string
}
