// navigator.clipboard 仅在安全上下文（HTTPS 或 localhost）下可用。
// 通过局域网 IP 等非安全上下文访问时 navigator.clipboard 为 undefined，
// 这里降级用 execCommand('copy') 保证复制功能仍可用。
export async function copyText(text: string): Promise<void> {
  if (navigator.clipboard) {
    await navigator.clipboard.writeText(text)
    return
  }

  const textarea = document.createElement('textarea')
  textarea.value = text
  textarea.style.position = 'fixed'
  textarea.style.left = '-9999px'
  document.body.appendChild(textarea)
  textarea.select()
  try {
    const ok = document.execCommand('copy')
    if (!ok) {
      throw new Error('execCommand copy failed')
    }
  } finally {
    document.body.removeChild(textarea)
  }
}
