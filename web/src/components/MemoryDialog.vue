<script setup lang="ts">
import { ref, watch } from 'vue'
import { api, toError } from '@/api/client'
import type { MemoryArchiveSummary } from '@/api/types'

const props = defineProps<{ open: boolean }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const archives = ref<MemoryArchiveSummary[]>([])
const selectedID = ref<number | null>(null)
const detail = ref('')
const totalChars = ref(0)
const nextOffset = ref(0)
const hasMore = ref(false)
const loadingList = ref(false)
const loadingDetail = ref(false)
const listError = ref('')
const detailError = ref('')
let detailEpoch = 0

watch(
  () => props.open,
  (open) => {
    if (open) void loadArchives()
    else {
      detailEpoch++
      selectedID.value = null
      detail.value = ''
      listError.value = ''
      detailError.value = ''
    }
  },
)

async function loadArchives(): Promise<void> {
  if (loadingList.value) return
  loadingList.value = true
  listError.value = ''
  try {
    const list = await api.listMemoryArchives()
    archives.value = list
    const selected = list.find((item) => item.conv_id === selectedID.value) ?? list[0]
    if (selected) await selectArchive(selected.conv_id)
    else {
      selectedID.value = null
      detail.value = ''
      totalChars.value = 0
      hasMore.value = false
    }
  } catch (err) {
    listError.value = toError(err).message
  } finally {
    loadingList.value = false
  }
}

async function selectArchive(id: number): Promise<void> {
  selectedID.value = id
  detail.value = ''
  totalChars.value = 0
  nextOffset.value = 0
  hasMore.value = false
  await loadDetail(id, 0, false)
}

async function loadDetail(id: number, offset: number, append: boolean): Promise<void> {
  if (loadingDetail.value) return
  const epoch = ++detailEpoch
  loadingDetail.value = true
  detailError.value = ''
  try {
    const res = await api.getMemoryArchive(id, offset)
    if (epoch !== detailEpoch || selectedID.value !== id) return
    detail.value = append ? detail.value + res.content : res.content
    totalChars.value = res.total_chars
    nextOffset.value = res.next_offset
    hasMore.value = res.has_more
  } catch (err) {
    if (epoch === detailEpoch) detailError.value = toError(err).message
  } finally {
    if (epoch === detailEpoch) loadingDetail.value = false
  }
}

function selectedArchive(): MemoryArchiveSummary | null {
  return archives.value.find((item) => item.conv_id === selectedID.value) ?? null
}

function formatTime(ms: number): string {
  if (!ms) return '尚未归档'
  return new Date(ms).toLocaleString('zh-CN', { hour12: false })
}

function onKeydown(e: KeyboardEvent): void {
  if (e.key === 'Escape') emit('close')
}
</script>

<template>
  <div v-if="open" class="modal" @click.self="emit('close')" @keydown="onKeydown">
    <div class="modal__panel memory-dialog" role="dialog" aria-modal="true" aria-label="查看记忆">
      <header class="modal__head">
        <span class="modal__title">长期记忆</span>
        <button type="button" class="icon-btn" title="关闭" @click="emit('close')">
          <svg width="13" height="13" viewBox="0 0 16 16" aria-hidden="true"><use href="#i-close" /></svg>
        </button>
      </header>

      <div class="memory-dialog__body">
        <aside class="memory-list">
          <div class="memory-list__head">
            <span>{{ archives.length }} 条归档</span>
            <button type="button" class="btn btn--sm" :disabled="loadingList" @click="loadArchives">
              <span v-if="loadingList" class="spinner" />刷新
            </button>
          </div>
          <div v-if="listError" class="notice notice--error">{{ listError }}</div>
          <div v-else-if="!archives.length && !loadingList" class="memory-empty">
            还没有已归档的记忆。开启 Session 的“记忆”后，内容会在每天 04:00 整理。
          </div>
          <button
            v-for="item in archives"
            :key="item.conv_id"
            type="button"
            class="memory-list__item"
            :class="{ 'memory-list__item--active': selectedID === item.conv_id }"
            @click="selectArchive(item.conv_id)"
          >
            <span class="memory-list__title">{{ item.title }}</span>
            <span class="memory-list__meta">Session #{{ item.conv_id }} · {{ formatTime(item.archived_at) }}</span>
            <span class="memory-list__meta">覆盖至消息 #{{ item.archived_before }}<template v-if="!item.enabled"> · 已停止写入</template></span>
            <span v-if="item.last_error" class="memory-list__error">{{ item.last_error }}</span>
          </button>
        </aside>

        <section class="memory-detail">
          <template v-if="selectedArchive()">
            <div class="memory-detail__head">
              <div>
                <h3>{{ selectedArchive()?.title }}</h3>
                <div class="hint">Session #{{ selectedID }} · 归档于 {{ formatTime(selectedArchive()?.archived_at ?? 0) }}</div>
              </div>
            </div>
          </template>
          <div v-if="detailError" class="notice notice--error">{{ detailError }}</div>
          <div v-else-if="loadingDetail && !detail" class="memory-empty">正在读取归档…</div>
          <pre v-else-if="detail" class="memory-detail__content">{{ detail }}</pre>
          <div v-else class="memory-empty">{{ archives.length ? '选择左侧条目查看详细归档。' : '暂无详细内容。' }}</div>
          <div v-if="hasMore" class="memory-detail__more">
            <button type="button" class="btn" :disabled="loadingDetail" @click="loadDetail(selectedID!, nextOffset, true)">
              <span v-if="loadingDetail" class="spinner" />加载更多（{{ detail.length }} / {{ totalChars }} 字）
            </button>
          </div>
        </section>
      </div>
    </div>
  </div>
</template>

<style scoped>
.memory-dialog {
  width: min(1060px, 94vw);
  height: min(760px, 88dvh);
  max-width: none;
}

.memory-dialog__body {
  display: grid;
  grid-template-columns: 310px minmax(0, 1fr);
  min-height: 0;
  flex: 1;
}

.memory-list {
  min-height: 0;
  overflow-y: auto;
  padding: var(--sp-3);
  border-right: 1px solid var(--border);
}

.memory-list__head,
.memory-detail__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
  margin-bottom: var(--sp-3);
  color: var(--text-muted);
  font-size: var(--fs-sm);
}

.memory-list__item {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  width: 100%;
  gap: 4px;
  padding: 10px;
  margin-bottom: 6px;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text);
  text-align: left;
  cursor: pointer;
}

.memory-list__item:hover,
.memory-list__item--active {
  border-color: var(--border);
  background: var(--bg-hover);
}

.memory-list__item--active {
  border-color: var(--accent);
}

.memory-list__title {
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-weight: 600;
}

.memory-list__meta,
.memory-list__error {
  color: var(--text-faint);
  font-size: var(--fs-xs);
}

.memory-list__error {
  color: var(--danger);
}

.memory-detail {
  min-width: 0;
  min-height: 0;
  overflow-y: auto;
  padding: var(--sp-4);
}

.memory-detail__head h3 {
  margin: 0 0 4px;
  font-size: var(--fs-lg);
}

.memory-detail__content {
  margin: 0;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  color: var(--text);
  font: inherit;
  font-size: var(--fs-md);
  line-height: 1.75;
}

.memory-detail__more {
  margin-top: var(--sp-4);
  text-align: center;
}

.memory-empty {
  padding: var(--sp-5) var(--sp-3);
  color: var(--text-faint);
  font-size: var(--fs-sm);
  line-height: 1.7;
  text-align: center;
}

@media (max-width: 720px) {
  .memory-dialog__body {
    grid-template-columns: 1fr;
    grid-template-rows: minmax(140px, 35%) minmax(0, 1fr);
  }

  .memory-list {
    border-right: 0;
    border-bottom: 1px solid var(--border);
  }
}
</style>
