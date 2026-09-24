// 回归测试：普通（未加密）共享不能被当成"锁住"，否则文件页的
// 上传 / 新建文件夹按钮会一直是禁用状态。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { isShareLocked, isShareUnlocked } from './share.ts'

const base = {
  id: 1,
  name: 'files',
  pool_id: 1,
  quota_mb: 0,
  compression: 0,
  encryption: 0,
  created_by: 1,
  created_at: 0,
  permission: 'admin',
  recycle_ttl_hours: 0,
  version_keep: 0,
}

test('plain share is never locked', () => {
  const plain = { ...base, encrypted: false, unlocked: false }
  assert.equal(isShareLocked(plain), false)
  assert.equal(isShareUnlocked(plain), false)
})

test('encrypted share is locked until it is unlocked', () => {
  const locked = { ...base, encrypted: true, unlocked: false, encryption: 1 }
  const unlocked = { ...base, encrypted: true, unlocked: true, encryption: 1 }
  assert.equal(isShareLocked(locked), true)
  assert.equal(isShareUnlocked(locked), false)
  assert.equal(isShareLocked(unlocked), false)
  assert.equal(isShareUnlocked(unlocked), true)
})

test('missing or partial share data is treated as usable', () => {
  assert.equal(isShareLocked(undefined), false)
  assert.equal(isShareLocked(null), false)
  assert.equal(isShareLocked({ ...base } as never), false) // 老后端缺字段时也不该锁死
})
