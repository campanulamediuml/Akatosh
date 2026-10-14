<script setup lang="ts">
/**
 * 会话系统提示词编辑弹窗。
 *
 * 从侧栏会话的「⋯」菜单进入。保存走 PATCH /api/conversations/{id}，
 * 后端会广播 conv 事件，两个 store 自动同步，这里只管发请求。
 */
import { nextTick, ref, watch } from 'vue'
import { api, toError } from '@/api/client'
import { editHint, handleEditKeydown } from '@/utils/editkeys'
import { toast } from '@/stores/toast'

const props = defineProps<{
  open: boolean
  /** 目标会话 id；null 时不该打开 */
  convId: number | null
  /** 打开时的当前值 */
  initial: string
  /** 会话标题（显示用） */
  title: string
}>()
const emit = defineEmits<{ (e: 'close'): void }>()

const draft = ref('')
const saving = ref(false)
const area = ref<HTMLTextAreaElement | null>(null)

watch(
  () => props.open,
  (open) => {
    if (!open) return
    draft.value = props.initial
    void nextTick(() => area.value?.focus())
  },
)

async function save(): Promise<void> {
  if (saving.value || props.convId === null) return
  saving.value = true
  try {
    await api.patchConversation(props.convId, { system_prompt: draft.value })
    toast.success(draft.value.trim() ? '系统提示词已保存' : '已清空系统提示词')
    emit('close')
  } catch (err) {
    toast.error(`保存失败：${toError(err).message}`)
  } finally {
    saving.value = false
  }
}

function onKeydown(e: KeyboardEvent): void {
  if (e.key === 'Escape') {
    e.preventDefault()
    emit('close')
    return
  }
  // Enter 行为与主输入框对齐（utils/editkeys.ts 里有中文输入法守卫）
  handleEditKeydown(e, () => void save())
}
</script>

<template>
  <div v-if="open" class="modal" @click.self="emit('close')">
    <div class="modal__panel modal__panel--narrow" role="dialog" aria-modal="true" aria-label="系统提示词">
      <header class="modal__head">
        <span class="modal__title">系统提示词</span>
        <button type="button" class="icon-btn" title="关闭" @click="emit('close')">
          <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
            <use href="#i-close" />
          </svg>
        </button>
      </header>

      <div class="modal__body">
        <p class="prompt__lead">
          只对「{{ title || '新对话' }}」生效，每轮对话都会带着它发给模型。留空 = 不用系统提示词。
        </p>
        <textarea
          ref="area"
          v-model="draft"
          class="input prompt__area"
          rows="6"
          spellcheck="false"
          placeholder="例如：你是一个数学助教。先推导再给结论，每一步推导都要严谨，不要给出似是而非的论证。"
          @keydown="onKeydown"
        />
      </div>

      <footer class="modal__foot">
        <span class="hint">{{ editHint() }}</span>
        <span class="modal__foot-spacer" />
        <button type="button" class="btn" :disabled="saving" @click="emit('close')">取消</button>
        <button type="button" class="btn btn--primary" :disabled="saving" @click="save">
          <span v-if="saving" class="spinner" />
          保存
        </button>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.prompt__lead {
  margin: 0;
  font-size: var(--fs-sm);
  color: var(--text-muted);
  line-height: 1.6;
}

.prompt__area {
  width: 100%;
  resize: vertical;
  min-height: 120px;
  font-size: var(--fs-md);
  line-height: 1.6;
}
</style>
