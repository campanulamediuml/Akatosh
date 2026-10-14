/**
 * 浏览器端 HEIC / HEIF → JPEG。
 *
 * ## 为什么这件事必须放在前端
 *
 * 后端是 Go：标准库和 `golang.org/x/image` **都没有 HEIF 解码器**，只认
 * png / jpg / gif / webp，所以 iPhone 直出的 HEIC 会被后端拒掉。
 * 但 HEIC 是苹果相机的默认格式，而**能解它的恰好就是拍出它的那台设备** ——
 * iOS / macOS 的 Safari 走系统 ImageIO，本来就能解 HEIF。
 *
 * 所以这里不需要引任何东西：用浏览器自带的解码器（`createImageBitmap` /
 * `<img>` + canvas）转一道，零依赖、零产物体积。
 *
 * ## 为什么不用 heic2any / libheif-js
 *
 * 解包体积分别是 **2.7 MB** 和 **9.0 MB**（npm dist.unpackedSize 实测），
 * 而当前整个前端产物才 1.28 MB。为了一个「只在 iOS 上才会遇到、而 iOS 自己就能解」
 * 的格式把包体翻两倍不划算。在解不了 HEIC 的浏览器（桌面 Chrome 等）上，
 * 这些函数会如实返回 null，由调用方给出准确的提示 ——
 * 而不是让后端回一句「这个文件不是能识别的图片」（文件明明是张照片）。
 *
 * ## 关于方向
 *
 * iPhone 拍的照片常常是「像素躺着 + EXIF 里记着要转 90°」。两条解码路径
 * 都显式要求按 from-image 处理，避免转出来是横的。
 */

/**
 * 长边上限。
 *
 * 不能设太大：iOS Safari 的 canvas **总像素**上限约 16.7M（4096×4096），
 * 48MP 的 iPhone 照片是 8064×6048 = 48.8M 像素，**根本画不进一块 canvas**，
 * 所以缩放不是可选项而是必须做的事。3840 既安全（3840² = 14.7M，留了余量），
 * 又对常见的 4032px 主摄几乎无损。
 */
const MAX_EDGE = 3840

/** 重编码质量。0.92 在肉眼无损和体积之间比较平衡 */
const JPEG_QUALITY = 0.92

/** 常见 HEIC/HEIF 的 MIME。iOS 上有时给空 MIME，所以还要看扩展名 */
const HEIC_TYPES = new Set([
  'image/heic',
  'image/heif',
  'image/heic-sequence',
  'image/heif-sequence',
])

/**
 * 是不是 HEIC / HEIF。
 *
 * MIME 优先；但 iOS 通过文件选择器给过来的 type 有时是空串，
 * Windows 资源管理器拖进来的也可能是空串或 application/octet-stream，
 * 所以这两种情况下退回看扩展名。
 */
export function isHeic(file: File): boolean {
  const type = (file.type || '').toLowerCase()
  if (HEIC_TYPES.has(type)) return true
  if (type !== '' && type !== 'application/octet-stream') return false
  return /\.(heic|heif)$/i.test(file.name)
}

type Decoded = ImageBitmap | HTMLImageElement

/**
 * 交给浏览器解码。两条路都试一下：
 * 1. `createImageBitmap` —— 不挂 DOM、可以显式声明按 EXIF 方向摆正
 * 2. `<img>` + objectURL —— 老 Safari 的兜底；现代浏览器渲染 <img> 时
 *    默认就按 EXIF 方向摆正，所以这里不用额外做什么
 *
 * 解不了就抛错，由上层转成 null。
 */
async function decode(file: File): Promise<Decoded> {
  if (typeof createImageBitmap === 'function') {
    try {
      return await createImageBitmap(file, { imageOrientation: 'from-image' })
    } catch {
      /* 落到 <img> 那条路 */
    }
  }

  const url = URL.createObjectURL(file)
  try {
    const img = new Image()
    await new Promise<void>((resolve, reject) => {
      img.onload = () => resolve()
      img.onerror = () => reject(new Error('浏览器解不了这张图'))
      img.src = url
    })
    return img
  } finally {
    // onload 之后像素已经在内存里了，objectURL 可以立刻回收
    URL.revokeObjectURL(url)
  }
}

/** photo.heic → photo.jpg */
function jpegName(name: string): string {
  const base = name.replace(/\.[^.]+$/, '')
  return `${base || 'photo'}.jpg`
}

/**
 * 把 HEIC 转成 JPEG。
 *
 * 返回 null 表示**这个浏览器解不了 HEIC**（或者 canvas / toBlob 不可用）——
 * 注意这跟「转换失败」不是一回事，调用方要给出不同的提示：
 * 前者是环境限制（换浏览器 / 改相机格式），后者才可能是图片坏了。
 */
export async function convertHeicToJpeg(file: File): Promise<File | null> {
  let src: Decoded
  try {
    src = await decode(file)
  } catch {
    return null
  }

  const w = src.width
  const h = src.height
  if (!w || !h) return null

  const scale = Math.min(1, MAX_EDGE / Math.max(w, h))
  const tw = Math.max(1, Math.round(w * scale))
  const th = Math.max(1, Math.round(h * scale))

  // 画到 canvas 再导成 JPEG。用 OffscreenCanvas 会更省主线程，
  // 但 Safari 的支持来得晚，这里不值得为它分两条路。
  const canvas = document.createElement('canvas')
  canvas.width = tw
  canvas.height = th
  const ctx = canvas.getContext('2d')
  if (!ctx) return null
  ctx.drawImage(src as CanvasImageSource, 0, 0, tw, th)
  if ('close' in src && typeof src.close === 'function') src.close()

  const blob = await new Promise<Blob | null>((resolve) => {
    canvas.toBlob(resolve, 'image/jpeg', JPEG_QUALITY)
  })
  if (!blob) return null

  return new File([blob], jpegName(file.name), {
    type: 'image/jpeg',
    lastModified: file.lastModified,
  })
}
