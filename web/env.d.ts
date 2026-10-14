/// <reference types="vite/client" />

// 让 .vue 单文件组件在 TS 里有类型（配合 vue-tsc / Volar 使用）。
declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<Record<string, unknown>, Record<string, unknown>, unknown>
  export default component
}
