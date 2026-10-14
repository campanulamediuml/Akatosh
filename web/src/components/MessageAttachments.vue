<script setup lang="ts">
/**
 * 消息里的图片网格（契约 §4）。
 *
 * 性能硬约束（改之前先读 API.md §4「前端硬约束」，别改回去）：
 * - 这里**只**请求 `thumb_url`（长边 320 的 JPEG，通常 10~30 KB）并加 `loading="lazy"`；
 * - `raw_url` 一个字都不出现——原图只在用户点开灯箱（ImageViewer）时才请求；
 * - 不做任何预加载：没有 `new Image()`，没有隐藏的 `<img>`，也没有 hover 预热。
 *   恢复聊天记录时最贵的就是原图，多图会话靠缩略图才快得起来。
 *
 * 布局：
 * - 一行最多 4 张，每格 120×120（列数写在 `atts--N` 类上），高度写死 →
 *   缩略图还在路上时也不会发生布局跳动；
 * - 没加载出来之前显示占位底色（`--bg-inset`）+ 转圈，加载失败显示警示图标。
 */
import { computed, ref } from 'vue'
import type { Attachment } from '@/api/types'
import ImageViewer from './ImageViewer.vue'

const props = defineProps<{ attachments: Attachment[] }>()

/** 一行最多几张 */
const MAX_COLS = 4

/** 网格列数：1 张就是 1 列（不会被拉成一整行），多于 4 张自动折行 */
const cols = computed(() => Math.max(1, Math.min(props.attachments.length, MAX_COLS)))

/** 缩略图加载状态，key 是 attachment id */
const loaded = ref<Record<number, boolean>>({})
const failed = ref<Record<number, boolean>>({})
function onLoad(id: number): void {
  loaded.value[id] = true
  failed.value[id] = false
}

function onError(id: number): void {
  failed.value[id] = true
}

/* ---------------- 灯箱 ---------------- */

/** null = 没打开；数字 = 看的是第几张 */
const viewerIndex = ref<number | null>(null)

function openViewer(i: number): void {
  viewerIndex.value = i
}

function closeViewer(): void {
  viewerIndex.value = null
}

function setViewerIndex(i: number): void {
  viewerIndex.value = i
}
</script>

<template>
  <div class="atts" :class="`atts--${cols}`">
    <!--
      key 里带下标：同一张图被重复添加时后端按 sha256 去重、会返回同一个 attachment id，
      只拿 id 当 key 就会撞（缩略图是同一张，展示不受影响）。
    -->
    <button
      v-for="(a, i) in attachments"
      :key="`${a.id}-${i}`"
      type="button"
      class="atts__item"
      :class="{ 'atts__item--loaded': loaded[a.id], 'atts__item--failed': failed[a.id] }"
      :title="`${a.name}（点击看大图）`"
      @click="openViewer(i)"
    >
      <!--
        唯一允许出现在消息列表里的图片地址：缩略图 + 懒加载。
        千万不要把 :src 换成 a.raw_url，那会让恢复历史变成下载全部原图。
      -->
      <img
        class="atts__img"
        :src="a.thumb_url"
        :alt="a.name"
        loading="lazy"
        decoding="async"
        @load="onLoad(a.id)"
        @error="onError(a.id)"
      />

      <span v-if="!loaded[a.id] && !failed[a.id]" class="atts__cover">
        <span class="spinner" />
      </span>
      <span v-else-if="failed[a.id]" class="atts__cover atts__cover--error" title="缩略图加载失败">
        <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
          <use href="#i-alert" />
        </svg>
      </span>
    </button>

    <!-- 灯箱只在点开时挂载 → raw_url 也只在那一刻才被请求 -->
    <ImageViewer
      v-if="viewerIndex !== null"
      :attachments="attachments"
      :index="viewerIndex"
      @close="closeViewer"
      @update:index="setViewerIndex"
    />
  </div>
</template>
