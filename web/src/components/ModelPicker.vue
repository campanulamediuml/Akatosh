<script setup lang="ts">
/**
 * Composer 右下角的两个下拉：模型 + 思考强度。
 *
 * 多供应商（契约 §4 的硬约束）：
 * - 模型列表**按 provider_id 分组**显示，每组一个小标题（供应商名）；
 * - 同名模型出现在两家供应商下时是两条不同的记录，必须能分别选到
 *   —— 所以本地用复合键 `${provider_id}:${model_id}` 判断选中态，
 *      点击时把 provider_id + model_id **一起** PATCH 回会话；
 * - 思考强度选项来自「当前会话那个模型」的 efforts，按 provider_id + model_id 一起找
 *   （只看 model_id 会串到别家的同名模型上）；空数组 = 没有档位可调 →
 *   整个下拉禁用；提示语区分「不会思考」（reasoning=false）和
 *   「思考默认开启、无档位」（reasoning=true 的开关式思考，如 MiMo）；
 * - 选项最前面固定一个「默认（不发参数）」（值为空串）：不下发 reasoning_effort，
 *   用上游自己的默认行为（像 deepseek-reasoner 这类模型本来就会思考，叫「关闭」不准确）。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import type { Model } from '@/api/types'
import { EFFORT_LABELS, modelKey } from '@/api/types'
import { useChatStore } from '@/stores/chat'
import { useSettingsStore } from '@/stores/settings'
import { abbreviateNumber } from '@/utils/format'

const chat = useChatStore()
const settings = useSettingsStore()

const root = ref<HTMLElement | null>(null)
const openMenu = ref<'' | 'model' | 'effort'>('')

/** 下拉里的一行：模型 + 它属于哪家（模板里用复合键做 :key，同名模型也不会撞） */
interface ModelOption {
  key: string
  model: Model
}

interface ModelGroup {
  providerId: number
  providerName: string
  options: ModelOption[]
}

function effortText(value: string): string {
  return EFFORT_LABELS[value] ?? value
}

/**
 * 当前会话用的供应商：会话自己绑定的那家；
 * 还没打开会话 / 后端给的是 0（未指定）时，退回「默认供应商」。
 */
const providerId = computed(
  () => chat.conv?.provider_id || settings.defaultProviderId || 0,
)

/** 当前会话用的模型名；没有打开的会话时跟随设置里的默认值 */
const modelId = computed(() => {
  const conv = chat.conv
  if (!conv) return settings.defaultModelId
  // 会话自己说用哪个就是哪个。model_id 为空时显示「选择模型」而**不猜**后端会退回
  // 哪个模型：退回的那条链路是「该会话的供应商下的第一个模型」，猜错会显示成别家的同名模型。
  return conv.model_id
})
const effort = computed(() => chat.conv?.effort ?? settings.defaults.effort ?? '')

/** 按 provider_id + model_id 一起找：同名模型不会被认成同一个 */
const currentModel = computed(() => settings.modelIn(providerId.value, modelId.value))
const currentProviderName = computed(() =>
  settings.providers.some((p) => p.id === providerId.value) ? settings.providerName(providerId.value) : '',
)

const efforts = computed(() => (currentModel.value?.efforts ?? []).filter((e) => !!e))
const supportsEffort = computed(() => efforts.value.length > 0)

/** 按供应商分组；供应商顺序跟随后端（id 升序） */
const groups = computed<ModelGroup[]>(() => {
  const out: ModelGroup[] = []
  const seen = new Set<number>()
  for (const p of settings.providers) {
    const models = settings.modelsOf(p.id)
    if (models.length === 0) continue
    seen.add(p.id)
    out.push({
      providerId: p.id,
      providerName: settings.providerName(p.id),
      options: models.map((m) => ({ key: modelKey(m.provider_id, m.model_id), model: m })),
    })
  }
  // 兜底：模型表里出现了已不在供应商列表里的 provider_id（供应商刚被删掉之类）也要能看见
  for (const m of settings.models) {
    if (seen.has(m.provider_id)) continue
    seen.add(m.provider_id)
    out.push({
      providerId: m.provider_id,
      providerName: settings.providerName(m.provider_id),
      options: settings
        .modelsOf(m.provider_id)
        .map((x) => ({ key: modelKey(x.provider_id, x.model_id), model: x })),
    })
  }
  return out
})

const modelLabel = computed(() => modelId.value || (settings.hasModels ? '选择模型' : '没有模型'))
const effortLabel = computed(() => {
  if (!modelId.value) return '先选择模型'
  if (!supportsEffort.value) {
    // reasoning=true 但 efforts 为空（如 MiMo：思考是开关式、默认开启）——
    // 和「不支持思考」（reasoning=false）区分开，别让前者看起来像不会思考
    return currentModel.value?.reasoning ? '思考默认开启（无档位）' : '该模型不支持思考强度'
  }
  return effort.value ? effortText(effort.value) : '默认'
})

/**
 * 生成途中切模型/供应商是允许的 —— 后端在发起这一轮时就把线路钉死了
 * （见 internal/session 的 generation.providerID），改道不了，切换只对下一轮生效。
 * 但界面上必须说清楚，否则用户会以为「我切了怎么还在用旧的」。
 *
 * 用「正在 streaming 的那条消息」而不是 session 级的 generating 对象：
 * 后者只在拉快照时填充，生成开始的那一刻不一定有值；而消息上的 provider_id /
 * model_id 是落库的权威记录，任何时候都对得上。
 */
const runningHint = computed(() => {
  const m = chat.streamingMessage
  if (!m) return ''
  if (m.provider_id === providerId.value && m.model_id === modelId.value) return ''
  const name = settings.providerName(m.provider_id)
  const where = name ? `${name} · ` : ''
  return `本轮仍在用 ${where}${m.model_id}，切换从下一轮生效`
})

const modelTriggerTitle = computed(() => {
  if (runningHint.value) return runningHint.value
  return currentProviderName.value ? `${currentProviderName.value} · ${modelLabel.value}` : '选择模型'
})

function isActive(m: Model): boolean {
  return m.provider_id === providerId.value && m.model_id === modelId.value
}

function toggle(which: 'model' | 'effort'): void {
  if (which === 'effort' && !supportsEffort.value) return
  openMenu.value = openMenu.value === which ? '' : which
}

function close(): void {
  openMenu.value = ''
}

/**
 * 选中一个模型。
 *
 * provider_id 和 model_id **必须一起写**：
 * 契约里只说 provider_id 的话后端会把 model_id 清空，所以这里从不下发单个 id。
 * 另外顺手把失效的 effort 清成空串（新模型没有声明这个强度时），
 * 免得会话里挂着一个该模型不认识的 reasoning_effort。
 */
async function pickModel(m: Model): Promise<void> {
  close()
  if (!chat.conv || isActive(m)) return
  const patch: { provider_id: number; model_id: string; effort?: string } = {
    provider_id: m.provider_id,
    model_id: m.model_id,
  }
  if (effort.value && !m.efforts.includes(effort.value)) patch.effort = ''
  await chat.patchConv(patch)
}

async function pickEffort(value: string): Promise<void> {
  close()
  if (!chat.conv || value === effort.value) return
  await chat.patchConv({ effort: value })
}

function onPointerDown(e: PointerEvent): void {
  const el = root.value
  if (!el) return
  if (e.target instanceof Node && el.contains(e.target)) return
  close()
}

onMounted(() => document.addEventListener('pointerdown', onPointerDown, true))
onBeforeUnmount(() => document.removeEventListener('pointerdown', onPointerDown, true))
</script>

<template>
  <div ref="root" class="composer__right">
    <!-- 模型（按供应商分组） -->
    <div class="picker">
      <button
        type="button"
        class="picker__trigger"
        :disabled="!settings.hasModels"
        :title="modelTriggerTitle"
        @click="toggle('model')"
      >
        <span v-if="currentProviderName" class="picker__provider">{{ currentProviderName }}</span>
        <span class="picker__label">{{ modelLabel }}</span>
        <svg class="picker__caret" width="11" height="11" viewBox="0 0 16 16" aria-hidden="true">
          <use href="#i-chevron-down" />
        </svg>
      </button>

      <div v-if="openMenu === 'model'" class="picker__menu">
        <div class="picker__menu-title">模型</div>
        <div v-if="runningHint" class="picker__hint">{{ runningHint }}</div>
        <template v-for="g in groups" :key="g.providerId">
          <div class="picker__group" :title="g.providerName">{{ g.providerName }}</div>
          <button
            v-for="opt in g.options"
            :key="opt.key"
            type="button"
            class="picker__item"
            :class="{ 'picker__item--active': isActive(opt.model) }"
            @click="pickModel(opt.model)"
          >
            <span class="picker__item-main">{{ opt.model.model_id }}</span>
            <span v-if="opt.model.context_len" class="picker__item-note">
              {{ abbreviateNumber(opt.model.context_len) }}
            </span>
            <svg
              v-if="isActive(opt.model)"
              class="picker__check"
              width="12"
              height="12"
              viewBox="0 0 16 16"
              aria-hidden="true"
            >
              <use href="#i-check" />
            </svg>
          </button>
        </template>
        <div v-if="groups.length === 0" class="picker__menu-title">还没有模型，去设置里获取</div>
      </div>
    </div>

    <!-- 思考强度 -->
    <div class="picker">
      <button
        type="button"
        class="picker__trigger"
        :disabled="!supportsEffort"
        :title="supportsEffort ? '思考强度' : effortLabel"
        @click="toggle('effort')"
      >
        <span class="picker__label">{{ effortLabel }}</span>
        <svg
          v-if="supportsEffort"
          class="picker__caret"
          width="11"
          height="11"
          viewBox="0 0 16 16"
          aria-hidden="true"
        >
          <use href="#i-chevron-down" />
        </svg>
      </button>

      <div v-if="openMenu === 'effort'" class="picker__menu">
        <div class="picker__menu-title">思考强度</div>
        <button
          type="button"
          class="picker__item"
          :class="{ 'picker__item--active': !effort }"
          @click="pickEffort('')"
        >
          <span class="picker__item-main">默认（不发参数）</span>
          <svg v-if="!effort" class="picker__check" width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-check" />
          </svg>
        </button>
        <button
          v-for="e in efforts"
          :key="e"
          type="button"
          class="picker__item"
          :class="{ 'picker__item--active': e === effort }"
          @click="pickEffort(e)"
        >
          <span class="picker__item-main">{{ effortText(e) }}（{{ e }}）</span>
          <svg v-if="e === effort" class="picker__check" width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-check" />
          </svg>
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* 触发按钮里的供应商名：只作提示，不抢模型名的位置 */
.picker__provider {
  flex: 0 0 auto;
  max-width: 76px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--text-faint);
  font-size: var(--fs-xs);
}

/* 分组小标题：一家供应商一段，同名模型因此能看出是两条不同的记录 */
.picker__group {
  padding: 6px var(--sp-2) 2px;
  font-size: var(--fs-xs);
  font-weight: 600;
  color: var(--text-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.picker__group:not(:first-of-type) {
  margin-top: 2px;
  border-top: 1px solid var(--border);
}

/* 生成中改了模型时的提示：说清楚「本轮不换、下一轮才生效」 */
.picker__hint {
  margin: 2px var(--sp-2) 6px;
  padding: 5px 7px;
  border-radius: 5px;
  background: var(--bg-soft, rgba(127, 127, 127, 0.12));
  color: var(--text-muted);
  font-size: var(--fs-xs);
  line-height: 1.45;
  white-space: normal;
}
</style>
