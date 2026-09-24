import type { ShareView } from './api'

/**
 * isShareLocked 判断共享当前是不是"用不了"。
 *
 * 注意：只有**加密**的共享才需要解锁。未加密的共享后端同样返回
 * unlocked=false（它压根没有密钥），拿 unlocked 单独判断会把普通共享
 * 误判成锁住——那样上传与新建文件夹按钮就永远是灰的。
 */
export function isShareLocked(share?: ShareView | null): boolean {
  return share?.encrypted === true && share.unlocked === false
}

/** isShareUnlocked 判断加密共享当前是否已在内存里解锁（未加密的共享返回 false）。 */
export function isShareUnlocked(share?: ShareView | null): boolean {
  return share?.encrypted === true && share.unlocked === true
}
