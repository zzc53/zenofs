// 首启向导里用来自动算出六位验证码，省得用户手工输入一遍。
// 参数与服务端完全一致：SHA-1 / 30 秒 / 6 位（RFC 6238）。

const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'

function base32Decode(input: string): Uint8Array {
  const clean = input.toUpperCase().replace(/[\s=]/g, '')
  let bits = 0
  let value = 0
  const out: number[] = []
  for (const ch of clean) {
    const idx = ALPHABET.indexOf(ch)
    if (idx < 0) throw new Error('bad base32 secret')
    value = (value << 5) | idx
    bits += 5
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 0xff)
      bits -= 8
    }
  }
  return new Uint8Array(out)
}

const BASE32_ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'

function base32Encode(bytes: Uint8Array): string {
  let bits = 0
  let value = 0
  let out = ''
  for (const b of bytes) {
    value = (value << 8) | b
    bits += 8
    while (bits >= 5) {
      out += BASE32_ALPHABET[(value >>> (bits - 5)) & 31]
      bits -= 5
    }
  }
  if (bits > 0) out += BASE32_ALPHABET[(value << (5 - bits)) & 31]
  return out
}

/** generateSecret 生成一个新的 base32 密钥（20 字节随机、无填充）。
 *  参数与服务端 internal/otp 完全一致，所以同一个密钥在两边算出的码相同。 */
export function generateSecret(): string {
  const bytes = new Uint8Array(20) // 20 字节 = SHA-1 的输出长度
  crypto.getRandomValues(bytes)
  return base32Encode(bytes)
}

/** otpauthURI 拼 otpauth:// 地址，字段与顺序都跟服务端 internal/otp.URI 对齐，
 *  这样扫码录入的密钥换到别的客户端也是同一套参数。 */
export function otpauthURI(secret: string, account: string, issuer = 'zenofs'): string {
  const label = issuer ? `${issuer}:${account}` : account
  // Go 的 url.PathEscape 不转义冒号，这里保持一致（否则两边的 URI 不一样）
  const escaped = encodeURIComponent(label).replace(/%3A/gi, ':')
  const q = new URLSearchParams()
  q.set('algorithm', 'SHA1')
  q.set('digits', '6')
  q.set('issuer', issuer)
  q.set('period', '30')
  q.set('secret', secret)
  return `otpauth://totp/${escaped}?${q.toString()}`
}

/** toArrayBuffer 拷贝一份真正的 ArrayBuffer（避免 SharedArrayBuffer 的类型麻烦）。 */
function toArrayBuffer(bytes: Uint8Array): ArrayBuffer {
  const buf = new ArrayBuffer(bytes.length)
  new Uint8Array(buf).set(bytes)
  return buf
}

/** totp 算出可选时刻的六位验证码（默认当前时间）。 */
export async function totp(secret: string, nowMs: number = Date.now()): Promise<string> {
  const key = toArrayBuffer(base32Decode(secret))
  const counter = Math.floor(nowMs / 30_000)
  const message = new ArrayBuffer(8)
  new DataView(message).setBigUint64(0, BigInt(counter))

  const cryptoKey = await crypto.subtle.importKey(
    'raw',
    key,
    { name: 'HMAC', hash: 'SHA-1' },
    false,
    ['sign'],
  )
  const mac = new Uint8Array(await crypto.subtle.sign('HMAC', cryptoKey, message))
  const offset = mac[mac.length - 1] & 0x0f
  const binary =
    ((mac[offset] & 0x7f) << 24) |
    (mac[offset + 1] << 16) |
    (mac[offset + 2] << 8) |
    mac[offset + 3]
  return String(binary % 1_000_000).padStart(6, '0')
}
