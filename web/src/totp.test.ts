// 前端的验证码必须和服务端 internal/otp 完全一致，否则首启向导会卡在第 1 步。
// 这里直接用 RFC 6238 的官方向量（SHA-1 密钥 "12345678901234567890"，取低 6 位）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { generateSecret, otpauthURI, totp } from './totp.ts'

const SECRET = 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ' // base32("12345678901234567890")

test('matches RFC 6238 vectors (SHA-1, 6 digits)', async () => {
  const cases: Array<[number, string]> = [
    [59, '287082'],
    [1111111109, '081804'],
    [1111111111, '050471'],
    [1234567890, '005924'],
    [2000000000, '279037'],
    [20000000000, '353130'],
  ]
  for (const [unix, want] of cases) {
    assert.equal(await totp(SECRET, unix * 1000), want, `t=${unix}`)
  }
})

test('is stable within a 30s step and rotates after it', async () => {
  const base = 1_700_000_000_000 // 落在某个步长中间
  const stepStart = Math.floor(base / 30_000) * 30_000
  assert.equal(await totp(SECRET, stepStart), await totp(SECRET, stepStart + 29_999))
  assert.notEqual(await totp(SECRET, stepStart), await totp(SECRET, stepStart + 30_000))
})

test('tolerates lowercase, spaces and padding in the secret', async () => {
  const canonical = await totp(SECRET, 59_000)
  for (const variant of [
    SECRET.toLowerCase(),
    'GEZD GNBV GY3T QOJQ GEZD GNBV GY3T QOJQ',
    SECRET + '====',
  ]) {
    assert.equal(await totp(variant, 59_000), canonical, variant)
  }
})

test('rejects malformed secrets', async () => {
  await assert.rejects(() => totp('not-base32!'))
})

test('generateSecret produces a usable base32 secret', async () => {
  const a = generateSecret()
  const b = generateSecret()
  assert.match(a, /^[A-Z2-7]+$/, '应当是 base32 字符集')
  assert.equal(a.length, 32, '20 字节 → 32 个字符（与服务端一致）')
  assert.notEqual(a, b, '每次都要不一样')
  // 生成的密钥立刻能算出六位码
  assert.equal((await totp(a, 59_000)).length, 6)
})

// 二维码里的地址必须和服务端 internal/otp.URI 完全一致（Go 侧用同一个期望值做精确断言）。
test('otpauth URI matches the server-side format', () => {
  assert.equal(
    otpauthURI('GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ', 'alice'),
    'otpauth://totp/zenofs:alice?algorithm=SHA1&digits=6&issuer=zenofs&period=30&secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ',
  )
})
