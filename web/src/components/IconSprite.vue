<script setup lang="ts">
/**
 * 全局 SVG 图标雪碧图。
 * 挂一次在 App.vue 顶部，其它组件用 <svg><use href="#i-xxx" /></svg> 引用即可。
 * 避免为了几个图标引入图标库，也避免在每个组件里重复内联 path。
 */
</script>

<template>
  <svg class="sr-only" aria-hidden="true" focusable="false" width="0" height="0">
    <defs>
      <symbol id="i-plus" viewBox="0 0 16 16">
        <path d="M8 3.2v9.6M3.2 8h9.6" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
      </symbol>
      <symbol id="i-sidebar" viewBox="0 0 16 16">
        <rect x="2" y="2.75" width="12" height="10.5" rx="1.6" fill="none" stroke="currentColor" stroke-width="1.3" />
        <path d="M6.2 3v10" fill="none" stroke="currentColor" stroke-width="1.3" />
      </symbol>
      <symbol id="i-globe" viewBox="0 0 16 16">
        <circle cx="8" cy="8" r="6.2" fill="none" stroke="currentColor" stroke-width="1.3" />
        <ellipse cx="8" cy="8" rx="2.9" ry="6.2" fill="none" stroke="currentColor" stroke-width="1.1" />
        <path d="M1.9 6.3h12.2M1.9 9.7h12.2" fill="none" stroke="currentColor" stroke-width="1.1" />
      </symbol>
      <symbol id="i-code" viewBox="0 0 16 16">
        <path d="M5.5 4.5L1.8 8l3.7 3.5M10.5 4.5L14.2 8l-3.7 3.5" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" />
      </symbol>
      <symbol id="i-doc" viewBox="0 0 16 16">
        <path d="M4 1.8h5.2L12 4.6v9.6H4z" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round" />
        <path d="M9 2v2.8h3" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round" />
        <path d="M5.8 7.4h4.4M5.8 10h4.4M5.8 12.6h2.8" fill="none" stroke="currentColor" stroke-width="1.1" stroke-linecap="round" />
      </symbol>
      <symbol id="i-image" viewBox="0 0 16 16">
        <rect x="1.8" y="2.6" width="12.4" height="10.8" rx="1.6" fill="none" stroke="currentColor" stroke-width="1.3" />
        <circle cx="5.4" cy="6.4" r="1.3" fill="none" stroke="currentColor" stroke-width="1.1" />
        <path d="M2.6 12.4l3.2-3.4 2.2 2.3 2.5-2.6 3.1 3.2" fill="none" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round" />
      </symbol>
      <symbol id="i-fork" viewBox="0 0 16 16">
        <circle cx="4" cy="3.5" r="1.8" fill="none" stroke="currentColor" stroke-width="1.2" />
        <circle cx="4" cy="12.5" r="1.8" fill="none" stroke="currentColor" stroke-width="1.2" />
        <circle cx="12" cy="6.5" r="1.8" fill="none" stroke="currentColor" stroke-width="1.2" />
        <path d="M4 5.3v5.4M12 8.3c0 2.5-3 2.7-5.5 2.7H4.6" fill="none" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
      </symbol>
      <symbol id="i-settings" viewBox="0 0 16 16">
        <circle cx="8" cy="8" r="2.1" fill="none" stroke="currentColor" stroke-width="1.3" />
        <path
          d="M8 1.6l1 1.6 1.86-.42.33 1.87 1.75.78-.94 1.66.94 1.66-1.75.78-.33 1.87-1.86-.42-1 1.6-1-1.6-1.86.42-.33-1.87-1.75-.78.94-1.66-.94-1.66 1.75-.78.33-1.87 1.86.42z"
          fill="none"
          stroke="currentColor"
          stroke-width="1.1"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-compress" viewBox="0 0 16 16">
        <!-- 上下箭头朝中间横线对挤：压缩上下文（横线 = 被压成的摘要） -->
        <path
          d="M3.5 8h9M8 1.6v4.2M5.7 3.6L8 5.9l2.3-2.3M8 14.4v-4.2M5.7 12.4L8 10.1l2.3 2.3"
          fill="none"
          stroke="currentColor"
          stroke-width="1.6"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-pencil" viewBox="0 0 16 16">
        <path
          d="M11.1 2.4l2.5 2.5-8 8-3.1.6.6-3.1z"
          fill="none"
          stroke="currentColor"
          stroke-width="1.3"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-trash" viewBox="0 0 16 16">
        <path
          d="M3 4.2h10M6.4 4.2V2.9h3.2v1.3M4.4 4.2l.6 8.2c0 .5.4.8.9.8h4.2c.5 0 .9-.3.9-.8l.6-8.2"
          fill="none"
          stroke="currentColor"
          stroke-width="1.3"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-pin" viewBox="0 0 16 16">
        <path
          d="M9.6 1.9l4.5 4.5-1.7 1.7-.6-.6-2.9 2.9.2 1.7-1.2 1.2-2.5-2.5-3 3-.7-.7 3-3-2.5-2.5L3.4 6.4l1.7.2 2.9-2.9-.6-.6z"
          fill="none"
          stroke="currentColor"
          stroke-width="1.2"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-pin-off" viewBox="0 0 16 16">
        <path d="M2.5 2.5l11 11" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" />
        <path
          d="M9.6 1.9l4.5 4.5-1.7 1.7-.6-.6-2.9 2.9.2 1.7-1.2 1.2-2.5-2.5"
          fill="none"
          stroke="currentColor"
          stroke-width="1.2"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-copy" viewBox="0 0 16 16">
        <rect x="5.4" y="5.4" width="8" height="8" rx="1.4" fill="none" stroke="currentColor" stroke-width="1.3" />
        <path d="M10.6 5.4V4a1.4 1.4 0 00-1.4-1.4H4A1.4 1.4 0 002.6 4v5.2A1.4 1.4 0 004 10.6h1.4" fill="none" stroke="currentColor" stroke-width="1.3" />
      </symbol>
      <symbol id="i-refresh" viewBox="0 0 16 16">
        <path
          d="M13.2 8a5.2 5.2 0 01-9 3.6M2.8 8a5.2 5.2 0 019-3.6"
          fill="none"
          stroke="currentColor"
          stroke-width="1.3"
          stroke-linecap="round"
        />
        <path d="M12 1.9v2.9H9.1M4 14.1v-2.9h2.9" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round" />
      </symbol>
      <symbol id="i-send" viewBox="0 0 16 16">
        <path
          d="M2.4 7.6l11.2-4.9-4.4 11-1.7-4.5z"
          fill="none"
          stroke="currentColor"
          stroke-width="1.3"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-stop" viewBox="0 0 16 16">
        <rect x="4.6" y="4.6" width="6.8" height="6.8" rx="1.2" fill="currentColor" />
      </symbol>
      <symbol id="i-chevron-right" viewBox="0 0 16 16">
        <path d="M6.2 3.6L10.6 8l-4.4 4.4" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
      </symbol>
      <symbol id="i-chevron-down" viewBox="0 0 16 16">
        <path d="M3.6 6.2L8 10.6l4.4-4.4" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
      </symbol>
      <symbol id="i-check" viewBox="0 0 16 16">
        <path d="M3.4 8.4l3 3 6.2-7" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
      </symbol>
      <symbol id="i-close" viewBox="0 0 16 16">
        <path d="M4 4l8 8M12 4l-8 8" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
      </symbol>
      <!-- 触屏下的「更多操作」入口：三个点，横排在中间 -->
      <symbol id="i-more" viewBox="0 0 16 16">
        <circle cx="3.4" cy="8" r="1.35" fill="currentColor" />
        <circle cx="8" cy="8" r="1.35" fill="currentColor" />
        <circle cx="12.6" cy="8" r="1.35" fill="currentColor" />
      </symbol>
      <symbol id="i-sun" viewBox="0 0 16 16">
        <circle cx="8" cy="8" r="3.1" fill="none" stroke="currentColor" stroke-width="1.3" />
        <path
          d="M8 1.4v1.6M8 13v1.6M1.4 8h1.6M13 8h1.6M3.3 3.3l1.1 1.1M11.6 11.6l1.1 1.1M12.7 3.3l-1.1 1.1M4.4 11.6l-1.1 1.1"
          fill="none"
          stroke="currentColor"
          stroke-width="1.3"
          stroke-linecap="round"
        />
      </symbol>
      <symbol id="i-moon" viewBox="0 0 16 16">
        <path
          d="M13.4 9.4A5.6 5.6 0 016.6 2.6a5.8 5.8 0 106.8 6.8z"
          fill="none"
          stroke="currentColor"
          stroke-width="1.3"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-alert" viewBox="0 0 16 16">
        <circle cx="8" cy="8" r="6.1" fill="none" stroke="currentColor" stroke-width="1.3" />
        <path d="M8 4.6v4.2" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
        <circle cx="8" cy="11.3" r="0.85" fill="currentColor" />
      </symbol>
      <symbol id="i-clock" viewBox="0 0 16 16">
        <circle cx="8" cy="8" r="6.1" fill="none" stroke="currentColor" stroke-width="1.3" />
        <path d="M8 4.4V8l2.6 1.6" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round" />
      </symbol>
      <symbol id="i-arrow-down" viewBox="0 0 16 16">
        <path d="M8 3.4v9M4.2 8.6L8 12.4l3.8-3.8" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" />
      </symbol>
      <symbol id="i-sparkle" viewBox="0 0 16 16">
        <path
          d="M8 1.8l1.5 4.2 4.2 1.5-4.2 1.5L8 13.2 6.5 9 2.3 7.5 6.5 6z"
          fill="none"
          stroke="currentColor"
          stroke-width="1.2"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-user" viewBox="0 0 16 16">
        <circle cx="8" cy="5.6" r="2.6" fill="none" stroke="currentColor" stroke-width="1.3" />
        <path d="M3.2 13.2a4.8 4.8 0 019.6 0" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" />
      </symbol>
      <symbol id="i-plug" viewBox="0 0 16 16">
        <path
          d="M6 1.8v3.4M10 1.8v3.4M4.2 5.2h7.6v2.2a3.8 3.8 0 01-7.6 0zM8 11.2v3"
          fill="none"
          stroke="currentColor"
          stroke-width="1.3"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-download" viewBox="0 0 16 16">
        <path
          d="M8 2.4v7.2M5 6.8L8 9.8l3-3M2.8 12.4h10.4"
          fill="none"
          stroke="currentColor"
          stroke-width="1.3"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-paperclip" viewBox="0 0 16 16">
        <path
          d="M10.9 4.6l-4.6 4.6a1.4 1.4 0 002 2l4.6-4.6a3 3 0 00-4.2-4.2L3.9 7.2a4.6 4.6 0 006.5 6.5l4.3-4.3"
          fill="none"
          stroke="currentColor"
          stroke-width="1.3"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </symbol>
      <symbol id="i-external" viewBox="0 0 16 16">
        <path d="M9.4 2.6h4v4" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round" />
        <path d="M13.4 2.6L7.6 8.4" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" />
        <path
          d="M12.2 9.4v2.8a1.2 1.2 0 01-1.2 1.2H3.8a1.2 1.2 0 01-1.2-1.2V5a1.2 1.2 0 011.2-1.2h2.8"
          fill="none"
          stroke="currentColor"
          stroke-width="1.3"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </symbol>
    </defs>
  </svg>
</template>
