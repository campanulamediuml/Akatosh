/**
 * 会话列表 + 当前会话 id。
 *
 * v2 分工：
 * - 这里只管「列表、选中、增删改」这些**列表层面**的事；
 * - 消息内容属于 stores/chat.ts，列表不碰；
 * - 谁打开会话 state 由 App.vue 监听 activeId 后调用 chat.openConversation()，
 *   这样依赖方向只有 chat -> conversations 一条，不会出现循环 import。
 *
 * `generating` 直接取后端给的值：会话列表、state 快照、以及每次生成开始/结束
 * 各广播一次的 conv 事件都会带上权威的 `conversation.generating`，
 * 所以侧栏脉冲小圆点不需要前端自己推断。
 */

import { defineStore } from 'pinia'
import { api, toError } from '@/api/client'
import type { Conversation } from '@/api/types'
import { toast } from './toast'

interface ConversationsState {
  items: Conversation[]
  activeId: number | null
  /** 首次加载（load）进行中 */
  loading: boolean
  /** 新建会话进行中 */
  creating: boolean
  /** 后台静默刷新进行中 */
  refreshing: boolean
  error: string
}

export const useConversationsStore = defineStore('conversations', {
  state: (): ConversationsState => ({
    items: [],
    activeId: null,
    loading: false,
    creating: false,
    refreshing: false,
    error: '',
  }),

  getters: {
    /** 置顶优先，其余按 updated_at 倒序（与后端的排序规则一致，做一层兜底） */
    sorted(state): Conversation[] {
      return [...state.items]
        .filter((c) => !c.archived)
        .sort((a, b) => {
          if (a.pinned !== b.pinned) return a.pinned ? -1 : 1
          return (b.updated_at ?? 0) - (a.updated_at ?? 0)
        })
    },

    active(state): Conversation | null {
      return state.items.find((c) => c.id === state.activeId) ?? null
    },
  },

  actions: {
    /* ---------------- 读取 ---------------- */

    /** 首次加载：拉列表；空列表就建一条；保证 activeId 有效 */
    async load(): Promise<void> {
      if (this.loading) return
      this.loading = true
      try {
        const ok = await this.refresh()
        // 列表没真正拉到（网络失败，或另一个刷新正在飞）时绝不盲目新建，
        // 否则一次抖动就可能建出第二个「新对话」
        if (!ok) return
        if (this.items.length === 0) {
          await this.create()
          return
        }
        if (this.activeId === null || !this.items.some((c) => c.id === this.activeId)) {
          this.activeId = this.sorted[0]?.id ?? null
        }
      } finally {
        this.loading = false
      }
    },

    /** 重新拉一次列表（重连后的全量重同步也会走这里）；失败只提示不抛出 */
    async refresh(): Promise<boolean> {
      if (this.refreshing) return false
      this.refreshing = true
      try {
        const list = await api.listConversations()
        const arr = Array.isArray(list) ? list : []
        this.items = arr
        if (this.activeId !== null && !arr.some((c) => c.id === this.activeId)) {
          this.activeId = this.sorted[0]?.id ?? null
        }
        this.error = ''
        return true
      } catch (err) {
        this.error = toError(err).message
        toast.error(`读取会话列表失败：${this.error}`)
        return false
      } finally {
        this.refreshing = false
      }
    },

    /* ---------------- 选中 ---------------- */

    select(id: number | null): void {
      this.activeId = id
    },

    /* ---------------- 增删改 ---------------- */

    /** 新建会话：后端会套用它保存的默认模型 / 思考强度，所以这里不传参 */
    async create(): Promise<Conversation | null> {
      if (this.creating) return null
      this.creating = true
      try {
        const conv = await api.createConversation({})
        if (conv && typeof conv.id === 'number') {
          this.upsertConv(conv)
          this.activeId = conv.id
          return conv
        }
        return null
      } catch (err) {
        toast.error(`新建会话失败：${toError(err).message}`)
        return null
      } finally {
        this.creating = false
      }
    },

    async rename(id: number, title: string): Promise<boolean> {
      const next = title.trim()
      if (!next) return false
      try {
        const res = await api.patchConversation(id, { title: next })
        if (res?.conv) this.upsertConv(res.conv, { preserveGenerating: true })
        return true
      } catch (err) {
        toast.error(`重命名失败：${toError(err).message}`)
        return false
      }
    },

    async togglePin(id: number): Promise<void> {
      const conv = this.items.find((c) => c.id === id)
      if (!conv) return
      try {
        const res = await api.patchConversation(id, { pinned: !conv.pinned })
        if (res?.conv) this.upsertConv(res.conv, { preserveGenerating: true })
      } catch (err) {
        toast.error(`置顶失败：${toError(err).message}`)
      }
    },

    /** 用户主动删除：删完若列表空了就再建一条，避免右侧空白 */
    async remove(id: number): Promise<void> {
      try {
        await api.deleteConversation(id)
      } catch (err) {
        toast.error(`删除会话失败：${toError(err).message}`)
        return
      }
      this.dropLocal(id)
      if (this.items.length === 0) await this.create()
    },

    /* ---------------- 事件与工具 ---------------- */

    /**
     * 按 id 覆盖或插入（conv 事件、PATCH 返回值、state 快照都走这里，幂等）。
     *
     * preserveGenerating：HTTP 的 PATCH 响应是数据库行（generating 恒为 false，
     * 后端紧接着广播的 conv 事件才是权威值）。调用方如果知道本地这份更可信，
     * 就传 true，避免脉冲小圆点闪一下。
     */
    upsertConv(conv: Conversation | null | undefined, opts?: { preserveGenerating?: boolean }): void {
      if (!conv || typeof conv.id !== 'number') return
      const idx = this.items.findIndex((c) => c.id === conv.id)
      let next = conv
      if (opts?.preserveGenerating && idx >= 0) {
        const cur = this.items[idx]
        if (cur?.generating && !conv.generating) next = { ...conv, generating: true }
      }
      if (idx >= 0) this.items.splice(idx, 1, next)
      else this.items.push(next)
    },

    /** conv 事件 action=deleted / state 404：从列表移除，并保证 activeId 有效 */
    handleConvDeleted(id: number): void {
      this.dropLocal(id)
    },

    /** 本地移除（不发请求） */
    dropLocal(id: number): void {
      const idx = this.items.findIndex((c) => c.id === id)
      if (idx >= 0) this.items.splice(idx, 1)
      if (this.activeId === id) this.activeId = this.sorted[0]?.id ?? null
    },

    /**
     * 标记会话已读（侧边栏小红点据此熄灭）。
     *
     * 先本地熄灭再发请求：这个操作很频繁（切会话、回答落地都会调），
     * 等一个来回再灭的话红点会闪一下。请求失败也无所谓 ——
     * 服务端下次返回的权威值会把它纠正回来。
     */
    async markRead(id: number): Promise<void> {
      const idx = this.items.findIndex((c) => c.id === id)
      const wasUnread = idx >= 0 && !!this.items[idx]?.unread
      if (wasUnread && idx >= 0) this.items.splice(idx, 1, { ...this.items[idx], unread: false })
      try {
        const res = await api.markConversationRead(id)
        if (res?.conv) this.upsertConv(res.conv, { preserveGenerating: true })
      } catch {
        // 静默：这只是一次"我看到了"的通知，失败了下次快照会纠正
      }
    },
  },
})
