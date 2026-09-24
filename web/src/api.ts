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
  /** 上传原始字节（文件内容就是 body）。 */
  putBlob: <T>(path: string, data: Blob) => request<T>('PUT', path, data),
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
