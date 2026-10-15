<script setup lang="ts">
/**
 * 左侧会话列表。
 * - 「+ 新对话」
 * - 每项三行：标题（含置顶/生成中脉冲/未读点）→ 供应商·时间·条数 → 操作图标行
 *   （重命名/系统提示词/置顶/分叉/删除，常驻显示——不再用悬停或「⋯」折叠，
 *   触屏的 sticky hover 问题和「按钮藏太深」问题一起消失）
 * - generating === true → 脉冲小圆点（后端在生成开始/结束都会广播 conv 事件，
 *   所以这里直接信 Conversation.generating，不做前端推断）
 */
import { nextTick, ref } from 'vue'
import type { ComponentPublicInstance } from 'vue'
import type { Conversation } from '@/api/types'
import { api } from '@/api/client'
import { useConversationsStore } from '@/stores/conversations'
import { toast } from '@/stores/toast'
import { relativeTime } from '@/utils/format'
import SystemPromptDialog from './SystemPromptDialog.vue'

const emit = defineEmits<{ (e: 'toggle'): void; (e: 'open-settings'): void }>()

const convs = useConversationsStore()

const renamingId = ref<number | null>(null)
const renameDraft = ref('')
/**
 * 注意：这个 input 在 v-for 里面，模板 ref 会被 Vue 收集成数组，
 * 所以用函数 ref（同一时刻只会渲染一个）而不是 ref="xxx"。
 */
const renameInput = ref<HTMLInputElement | null>(null)

function setRenameInput(el: Element | ComponentPublicInstance | null): void {
  renameInput.value = el instanceof HTMLInputElement ? el : null
}

const confirmId = ref<number | null>(null)

function onTogglePin(c: Conversation): void {
  confirmId.value = null
  void convs.togglePin(c.id)
}

/** 分叉：全量值复制整个会话（后端复制 messages/附件关联/配置），完成后切到新会话 */
async function onFork(c: Conversation): Promise<void> {
  confirmId.value = null
  try {
    const res = await api.forkConversation(c.id)
    toast.success(`已分叉为「${res?.conv?.title ?? c.title + '-fork'}」`)
    await convs.refresh()
    if (res?.conv?.id) convs.select(res.conv.id)
  } catch (err) {
    toast.error(`分叉失败：${err instanceof Error ? err.message : String(err)}`)
  }
}

function onSelect(c: Conversation): void {
  if (renamingId.value === c.id) return
  confirmId.value = null
  if (convs.activeId !== c.id) convs.select(c.id)
}

/* ---------------- 重命名 ---------------- */

function startRename(c: Conversation): void {
  confirmId.value = null
  renamingId.value = c.id
  renameDraft.value = c.title ?? ''
  void nextTick(() => {
    renameInput.value?.focus()
    renameInput.value?.select()
  })
}

function cancelRename(): void {
  renamingId.value = null
}

/** Enter / 失焦提交；内容没变就当作取消 */
async function commitRename(): Promise<void> {
  const id = renamingId.value
  if (id === null) return
  const draft = renameDraft.value
  renamingId.value = null
  const conv = convs.items.find((c) => c.id === id)
  if (!conv || draft.trim() === (conv.title ?? '').trim()) return
  await convs.rename(id, draft)
}

function onRenameKeydown(e: KeyboardEvent): void {
  // 中文输入法守卫：选词时的回车不能当成保存
  if (e.isComposing || e.keyCode === 229) return
  if (e.key === 'Enter') {
    e.preventDefault()
    void commitRename()
  } else if (e.key === 'Escape') {
    e.preventDefault()
    cancelRename()
  }
}

/* ---------------- 系统提示词 ---------------- */

/** 正在编辑系统提示词的会话（弹窗用） */
const promptConv = ref<Conversation | null>(null)

function openPrompt(c: Conversation): void {
  confirmId.value = null

  promptConv.value = c
}

function closePrompt(): void {
  promptConv.value = null
}

/* ---------------- 删除（二次确认） ---------------- */

function askDelete(id: number): void {
  renamingId.value = null

  confirmId.value = confirmId.value === id ? null : id
}

function cancelDelete(): void {
  confirmId.value = null
}

function doDelete(id: number): void {
  confirmId.value = null
  void convs.remove(id)
}
</script>

<template>
  <aside class="sidebar">
    <div class="sidebar__head">
      <span class="sidebar__brand">Akatosh</span>
      <!-- 设置是全局的（供应商/默认提示词…），不放 session 窗口里 -->
      <button type="button" class="icon-btn" title="设置" @click="emit('open-settings')">
        <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
          <use href="#i-settings" />
        </svg>
      </button>
      <button type="button" class="icon-btn" title="折叠侧栏" @click="emit('toggle')">
        <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
          <use href="#i-sidebar" />
        </svg>
      </button>
    </div>

    <button
      type="button"
      class="btn btn--block sidebar__new"
      :disabled="convs.creating"
      @click="convs.create()"
    >
      <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true">
        <use href="#i-plus" />
      </svg>
      新对话
    </button>

    <nav class="sidebar__list" aria-label="会话列表">
      <div v-if="convs.sorted.length === 0" class="sidebar__empty">还没有会话</div>

      <div
        v-for="c in convs.sorted"
        :key="c.id"
        class="conv"
        :class="{
          'conv--active': c.id === convs.activeId,
          'conv--menu-open': confirmId === c.id,
        }"
        :data-conv-id="c.id"
        :data-unread="c.unread ? '1' : '0'"
        @click="onSelect(c)"
      >
        <div v-if="renamingId === c.id" class="conv__rename" @click.stop>
          <input
            :ref="setRenameInput"
            v-model="renameDraft"
            class="input"
            type="text"
            placeholder="会话标题"
            @keydown="onRenameKeydown"
            @blur="commitRename"
          />
        </div>

        <template v-else>
          <!-- 第一行：标题 + 状态 -->
          <div class="conv__row">
            <span v-if="c.pinned" class="conv__pin" title="已置顶">
              <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-pin" />
              </svg>
            </span>
            <span class="conv__title" :title="c.title">{{ c.title || '新对话' }}</span>
            <span v-if="c.generating" class="pulse" title="正在生成回复" />
            <span
              v-else-if="c.unread && c.id !== convs.activeId"
              class="conv__unread"
              title="有新的回复"
            />
          </div>

          <!-- 第二行：供应商 · 时间 · 条数（删除确认态替换本行） -->
          <div v-if="confirmId === c.id" class="conv__confirm" @click.stop>
            <span>删除这个会话？</span>
            <button type="button" class="btn btn--sm btn--danger" @click="doDelete(c.id)">删除</button>
            <button type="button" class="btn btn--sm" @click="cancelDelete">取消</button>
          </div>
          <div v-else class="conv__meta">
            <span v-if="c.provider_name" class="conv__meta-prov" :title="c.provider_name">
              {{ c.provider_name }}
            </span>
            <span v-if="c.provider_name" class="conv__meta-dot" />
            <span>{{ relativeTime(c.updated_at) }}</span>
            <span class="conv__meta-dot" />
            <span>{{ c.message_count }} 条</span>
          </div>

          <!-- 第三行：操作图标（常驻，不再 hover/⋯） -->
          <div class="conv__ops">
            <button type="button" class="icon-btn" title="重命名" @click.stop="startRename(c)">
              <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-pencil" />
              </svg>
            </button>
            <button
              type="button"
              class="icon-btn"
              :class="{ 'icon-btn--active': !!c.system_prompt }"
              :title="c.system_prompt ? '系统提示词（已设置）' : '系统提示词'"
              @click.stop="openPrompt(c)"
            >
              <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-doc" />
              </svg>
            </button>
            <button
              type="button"
              class="icon-btn"
              :title="c.pinned ? '取消置顶' : '置顶'"
              @click.stop="onTogglePin(c)"
            >
              <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
                <use :href="c.pinned ? '#i-pin-off' : '#i-pin'" />
              </svg>
            </button>
            <button type="button" class="icon-btn" title="分叉（复制整个会话开新分支）" @click.stop="onFork(c)">
              <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-fork" />
              </svg>
            </button>
            <button
              type="button"
              class="icon-btn icon-btn--danger"
              title="删除会话"
              @click.stop="askDelete(c.id)"
            >
              <svg width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
                <use href="#i-trash" />
              </svg>
            </button>
          </div>
        </template>
      </div>
    </nav>

    <SystemPromptDialog
      :open="promptConv !== null"
      :conv-id="promptConv?.id ?? null"
      :initial="promptConv?.system_prompt ?? ''"
      :title="promptConv?.title ?? ''"
      @close="closePrompt"
    />
  </aside>
</template>

<style scoped>
/* 供应商名只是个提示，太长就截断，把时间/条数留在原位 */
.conv__meta-prov {
  min-width: 0;
  max-width: 84px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/*
  未读小红点：实心、不闪。
  和「正在生成」的蓝色脉冲点刻意区分开 —— 那个是「在动」，这个是「等你来看」。
*/
.conv__unread {
  flex: 0 0 auto;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--danger);
  box-shadow: 0 0 0 2px var(--danger-soft);
}
</style>
