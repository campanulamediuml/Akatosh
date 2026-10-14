/**
 * 流式渲染节流：生成中每帧都全量重渲染 markdown 会把 CPU 打满
 * （实测：26 万字 CoT 时持续 1.2 核），这里把「传给渲染组件的文本」
 * 在流式期间节流到每 500ms 一次，结束瞬间立即同步最终全文。
 */
import { onBeforeUnmount, ref, watch, type Ref } from 'vue'

/**
 * source 是响应式文本的 getter（比如 props.message.content）；
 * active 为 true 时（流式中）节流，false 时直通。
 * active 从 true 跳 false（生成结束）时立刻同步一次，保证最终内容完整。
 */
export function useThrottledText(source: () => string, active: Ref<boolean>, intervalMs = 500): Ref<string> {
  const out = ref(source())
  let timer: ReturnType<typeof setInterval> | null = null
  let dirty = false

  const stop = (): void => {
    if (timer !== null) {
      clearInterval(timer)
      timer = null
    }
  }

  watch(source, () => {
    if (!active.value) {
      out.value = source()
      return
    }
    dirty = true
    if (timer === null) {
      timer = setInterval(() => {
        if (dirty) {
          out.value = source()
          dirty = false
        }
      }, intervalMs)
    }
  })

  watch(active, (a) => {
    if (!a) {
      stop()
      out.value = source()
    }
  })

  onBeforeUnmount(stop)
  return out
}
