/**
 * 构建后把 dist/sw.js 里的 __SW_BUILD__ 替换成当次构建的内容 hash
 * （取主 JS 文件名里的那段 hash —— 它就是这次前端内容的指纹）。
 *
 * 没有这一步，sw.js 只有在手工改动时才会变；前端业务代码更新后浏览器
 * 的 SW 更新检查（字节对比）会认为「没变」，旧缓存一直留存。
 * 注入后每次构建 sw.js 必然变化：install 预缓存新入口、activate 清掉上一版缓存。
 */
import { readdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

// 注意必须 fileURLToPath：项目路径含中文，URL.pathname 会 percent-encode
const dist = fileURLToPath(new URL('../dist/', import.meta.url))
const assetsDir = join(dist, 'assets')

const mainJs = readdirSync(assetsDir).find((f) => /^index-[^.]+\.js$/.test(f))
if (!mainJs) {
  console.error('[inject-sw-version] 找不到 dist/assets/index-*.js')
  process.exit(1)
}
const buildHash = mainJs.match(/^index-([^.]+)\.js$/)[1]

const swPath = join(dist, 'sw.js')
const sw = readFileSync(swPath, 'utf8')
if (!sw.includes('__SW_BUILD__')) {
  console.error('[inject-sw-version] sw.js 里没有 __SW_BUILD__ 占位（已被替换过？）')
  process.exit(1)
}
writeFileSync(swPath, sw.replaceAll('__SW_BUILD__', buildHash))
console.log(`[inject-sw-version] sw.js 版本戳 → ${buildHash}`)
