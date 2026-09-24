import type { ComponentChildren } from 'preact'
import qrcode from 'qrcode-generator'
import { lang, setLang } from './i18n'

// 共享的小组件与格式化工具（保持轻量，不引第三方 UI 库）。

/** LangSwitch 是语言切换器；登录页与向导页也要用到。 */
export function LangSwitch() {
  return (
    <select
      class="lang"
      value={lang.value}
      onChange={(e) => setLang(e.currentTarget.value as 'en' | 'zh')}
    >
      <option value="en">English</option>
      <option value="zh">中文</option>
    </select>
  )
}

/** Modal 是一个居中的对话框。 */
export function Modal(props: {
  title: string
  onClose: () => void
  children: ComponentChildren
  footer?: ComponentChildren
}) {
  return (
    <div class="overlay" onClick={props.onClose}>
      <div class="modal" onClick={(e) => e.stopPropagation()}>
        <header>
          <h3>{props.title}</h3>
          <button class="btn-secondary" onClick={props.onClose}>
            ✕
          </button>
        </header>
        <div class="modal-body">{props.children}</div>
        {props.footer && <footer class="modal-foot">{props.footer}</footer>}
      </div>
    </div>
  )
}

/** Field 是"标签 + 控件 + 可选提示"的竖排表单项。 */
export function Field(props: { label: string; hint?: string; children: ComponentChildren }) {
  return (
    <label class="field">
      <span class="field-label">{props.label}</span>
      {props.children}
      {props.hint && <span class="field-hint">{props.hint}</span>}
    </label>
  )
}

/** Empty 是"这里没有内容"的占位。 */
export function Empty(props: { text: string }) {
  return <p class="empty">{props.text}</p>
}

/** ErrorBox 展示一条错误。 */
export function ErrorBox(props: { text: string }) {
  if (!props.text) return null
  return <div class="error">{props.text}</div>
}

/** fmtBytes 把字节数变成人类可读的大小。 */
export function fmtBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '-'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let value = n
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value >= 100 || unit === 0 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`
}

/** fmtTime 把 Unix 秒转成可读时间（0 表示"从未/永不过期"，由调用方决定怎么显示）。 */
export function fmtTime(unix: number): string {
  if (!unix) return '-'
  return new Date(unix * 1000).toLocaleString()
}

/** copyText 复制到剪贴板（失败时静默，UI 仍然显示内容）。 */
export async function copyText(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
  } catch {
    // 非安全上下文或没有权限：忽略
  }
}

/** QrCode 把一段文本画成 SVG。用 qrcode-generator：纯前端、不联网、没有运行时依赖，
 *  密钥只留在页面里，不会出现在任何 URL 或访问日志中。 */
export function QrCode(props: { text: string; width?: number }) {
  let svg = ''
  try {
    const qr = qrcode(0, 'M') // 0 = 自动选版本，M = 纠错级别
    qr.addData(props.text)
    qr.make()
    svg = qr.createSvgTag(4, 2)
  } catch {
    // 文本异常时不显示二维码，密钥仍然可以手工录入
    return null
  }
  return (
    <div
      class="qrcode"
      style={props.width ? `width:${props.width}px` : undefined}
      dangerouslySetInnerHTML={{ __html: svg }}
    />
  )
}
