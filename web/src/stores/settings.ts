/**
 * 多供应商：供应商列表 / 每家名下的模型 / 新建会话的默认值。
 *
 * v2 契约（API.md §4）与 v1 的关键区别：
 * - 供应商是一张**列表**（GET /api/providers），不再是「一个 provider」；
 * - 拉模型 / 测试连接都是**针对某个 id** 的操作：
 *   POST /api/providers/{id}/models/fetch、POST /api/providers/{id}/test；
 * - GET /api/settings 返回 `{providers, defaults}`，defaults 里多了 provider_id；
 * - PUT /api/settings/defaults 也能改 provider_id。
 *
 * 「获取模型列表」「测试连接」的结果按供应商 id 分开存在 store 里，
 * 这样它们可以显示在**各自那一行**下面，而不是弹窗顶部的一团。
 *
 * 默认值存在后端，前端只是镜像：新建会话时连 model_id 都不用传，后端自己套用。
 */

import { defineStore } from 'pinia'
import { api, toError } from '@/api/client'
import type {
  CreateProviderInput,
  DefaultsInput,
  Me,
  Model,
  Provider,
  ProviderTestResponse,
  SettingsDefaults,
  SettingsResponse,
  UpdateProviderInput,
  UserInfo,
} from '@/api/types'
import { toast } from './toast'

export const EMPTY_DEFAULTS: SettingsDefaults = { provider_id: 0, model_id: '', effort: '' }

export interface SaveResult {
  ok: boolean
  message: string
}

/** 某一家「获取模型列表」的结果：成功给总数/新增，失败给后端 error.message */
export interface FetchModelsOutcome {
  ok: boolean
  total: number
  added: number
  message: string
}

interface SettingsState {
  loading: boolean
  loaded: boolean
  loadError: string

  providers: Provider[]
  defaults: SettingsDefaults
  /** 所有供应商的模型（GET /api/models，每项带 provider_id） */
  models: Model[]
  /** 后端是否配置了搜索后端（-searxng）；false 时 Composer 不显示「联网」开关 */
  webSearchAvailable: boolean
  /** 上下文压缩阈值（0~1，0 = 禁用） */
  compactThreshold: number

  /** 当前登录用户（GET /api/me）；null = 还没拉过 */
  me: Me | null

  /** 用户管理（管理员）：用户列表与操作态 */
  users: UserInfo[]
  loadingUsers: boolean

  /** 新增 / 编辑供应商表单的保存中 */
  savingProvider: boolean
  /** 正在删除的供应商 id */
  deletingId: number | null
  savingDefaults: boolean

  /** 正在「获取模型列表」的供应商 id */
  fetchingIds: number[]
  /** 正在「测试连接」的供应商 id */
  testingIds: number[]
  /** 每家一次「获取模型列表」的结果 */
  fetchResults: Record<number, FetchModelsOutcome>
  /** 每家一次「测试连接」的结果 */
  testResults: Record<number, ProviderTestResponse>
}

function normalizeDefaults(raw: Partial<SettingsDefaults> | null | undefined): SettingsDefaults {
  return {
    provider_id: typeof raw?.provider_id === 'number' && Number.isFinite(raw.provider_id) ? raw.provider_id : 0,
    model_id: typeof raw?.model_id === 'string' ? raw.model_id : '',
    effort: typeof raw?.effort === 'string' ? raw.effort : '',
    system_prompt: typeof raw?.system_prompt === 'string' ? raw.system_prompt : '',
  }
}

export const useSettingsStore = defineStore('settings', {
  state: (): SettingsState => ({
    loading: false,
    loaded: false,
    loadError: '',
    providers: [],
    defaults: { ...EMPTY_DEFAULTS },
    models: [],
    webSearchAvailable: false,
    compactThreshold: 0,
    me: null,
    users: [],
    loadingUsers: false,
    savingProvider: false,
    deletingId: null,
    savingDefaults: false,
    fetchingIds: [],
    testingIds: [],
    fetchResults: {},
    testResults: {},
  }),

  getters: {
    /** 当前用户是否管理员（决定「用户管理」入口的显隐） */
    isAdmin(state): boolean {
      return state.me?.is_admin === true
    },

    /** 当前用户名（界面角落展示用；没拉到就空串） */
    userName(state): string {
      return state.me?.name ?? ''
    },

    /** 至少配了一家供应商（有的上游不需要 key，所以只看有没有记录） */
    hasProvider(state): boolean {
      return state.providers.length > 0
    },

    hasModels(state): boolean {
      return state.models.length > 0
    },

    /** 输入框是否可用：有供应商 + 至少一个模型 */
    isConfigured(): boolean {
      return this.hasProvider && this.hasModels
    },

    providerById(state): (id: number) => Provider | null {
      return (id: number) => state.providers.find((p) => p.id === id) ?? null
    },

    /** 供应商显示名：后端允许 name 为空，退化成「供应商 #id」 */
    providerName(state): (id: number) => string {
      return (id: number) => {
        const p = state.providers.find((x) => x.id === id)
        if (p && p.name.trim()) return p.name.trim()
        return id ? `供应商 #${id}` : '未指定供应商'
      }
    },

    /** 某一家名下的模型（按 provider_id 过滤） */
    modelsOf(state): (providerId: number) => Model[] {
      return (providerId: number) => state.models.filter((m) => m.provider_id === providerId)
    },

    /**
     * 按「供应商 + 模型名」精确定位一条模型记录。
     * 同名模型在不同供应商下是两条记录，所以只给 model_id 是不够的。
     */
    modelIn(state): (providerId: number, modelId: string) => Model | null {
      return (providerId: number, modelId: string) =>
        state.models.find((m) => m.provider_id === providerId && m.model_id === modelId) ?? null
    },

    /**
     * 新建会话会用的供应商。
     * 后端给的 provider_id 在列表里找不到时（供应商被删掉等）退化成第一家；
     * 返回值可能为 0 = 一家都没有。
     */
    defaultProviderId(state): number {
      if (state.providers.some((p) => p.id === state.defaults.provider_id)) return state.defaults.provider_id
      return state.providers[0]?.id ?? 0
    },

    defaultProvider(): Provider | null {
      return this.providerById(this.defaultProviderId)
    },

    /** 默认供应商名下的模型（默认模型下拉的选项） */
    defaultProviderModels(): Model[] {
      return this.modelsOf(this.defaultProviderId)
    },

    /** 下拉里真正被选中的模型 id；不在默认供应商名下时是空串 */
    defaultModelId(state): string {
      const list = this.models.filter((m) => m.provider_id === this.defaultProviderId)
      return list.some((m) => m.model_id === state.defaults.model_id) ? state.defaults.model_id : ''
    },

    defaultModel(): Model | null {
      return this.modelIn(this.defaultProviderId, this.defaultModelId)
    },

    /** 默认模型的思考强度选项（空数组 = 不支持思考） */
    defaultEfforts(): string[] {
      return this.defaultModel?.efforts ?? []
    },

    isFetching(state): (id: number) => boolean {
      return (id: number) => state.fetchingIds.includes(id)
    },

    isTesting(state): (id: number) => boolean {
      return (id: number) => state.testingIds.includes(id)
    },
  },

  actions: {
    /* ---------------- 读取 ---------------- */

    async load(): Promise<void> {
      if (this.loading) return
      this.loading = true
      try {
        await Promise.all([this.loadMe(), this.loadSettings(), this.loadModels()])
      } finally {
        this.loading = false
        this.loaded = true
      }
    },

    /** GET /api/me：当前登录用户（管理员标记决定「用户管理」入口显隐） */
    async loadMe(): Promise<void> {
      try {
        this.me = await api.getMe()
      } catch {
        this.me = null
      }
    },

    /* ---------------- 用户管理（管理员） ---------------- */

    /** 拉用户列表（非管理员会得到 403，静默吞掉——入口本来就藏着） */
    async loadUsers(): Promise<void> {
      if (!this.isAdmin) return
      this.loadingUsers = true
      try {
        this.users = await api.listUsers()
      } catch (err) {
        toast.error(`加载用户列表失败：${toError(err).message}`)
      } finally {
        this.loadingUsers = false
      }
    },

    /** 创建用户：返回明文令牌（只在创建这一刻可见，必须当场复制） */
    async createUser(name: string): Promise<string | null> {
      try {
        const res = await api.createUser(name)
        await this.loadUsers()
        return res.token
      } catch (err) {
        toast.error(`创建用户失败：${toError(err).message}`)
        return null
      }
    },

    /** 重置令牌：旧令牌立即失效；返回的新令牌同样只可见这一次 */
    async rotateToken(id: number): Promise<string | null> {
      try {
        const res = await api.rotateUserToken(id)
        return res.token
      } catch (err) {
        toast.error(`重置令牌失败：${toError(err).message}`)
        return null
      }
    },

    async deleteUser(id: number): Promise<boolean> {
      try {
        await api.deleteUser(id)
        this.users = this.users.filter((u) => u.id !== id)
        return true
      } catch (err) {
        toast.error(`删除用户失败：${toError(err).message}`)
        return false
      }
    },

    /** GET /api/settings：供应商列表 + 默认值 */
    async loadSettings(): Promise<void> {
      try {
        const res = await api.getSettings()
        this.applySettingsResponse(res)
        this.loadError = ''
      } catch (err) {
        this.loadError = toError(err).message
        toast.error(`读取设置失败：${this.loadError}`)
      }
    },

    /** GET /api/models：所有供应商的模型 */
    async loadModels(): Promise<void> {
      try {
        const list = await api.listModels()
        this.models = Array.isArray(list) ? list : []
      } catch (err) {
        // 没有模型不是致命错误（可能还没配过任何供应商），只记录不弹 toast
        this.loadError = toError(err).message
      }
    },

    /** 供应商/模型/默认值都可能一起变（PUT /api/settings/defaults 也返回这个形状） */
    applySettingsResponse(res: SettingsResponse | null | undefined): void {
      this.providers = Array.isArray(res?.providers) ? res.providers : []
      this.defaults = normalizeDefaults(res?.defaults)
      this.webSearchAvailable = res?.web_search_available === true
      this.compactThreshold = typeof res?.compact_threshold === 'number' ? res.compact_threshold : 0
    },

    async reload(): Promise<void> {
      await Promise.all([this.loadSettings(), this.loadModels()])
    },

    /* ---------------- 供应商 CRUD ---------------- */

    async createProvider(input: CreateProviderInput): Promise<SaveResult> {
      return this.writeProvider(() => api.createProvider(input), '已添加供应商')
    },

    async updateProvider(id: number, input: UpdateProviderInput): Promise<SaveResult> {
      return this.writeProvider(() => api.patchProvider(id, input), '已保存供应商')
    },

    /** 新增 / 编辑的公共外壳：成功后就地重拉（模型数、默认值都可能被后端顺手改了） */
    async writeProvider(fn: () => Promise<Provider>, okMessage: string): Promise<SaveResult> {
      this.savingProvider = true
      try {
        await fn()
        await this.reload()
        toast.success(okMessage)
        return { ok: true, message: '' }
      } catch (err) {
        const message = toError(err).message
        toast.error(`保存失败：${message}`)
        return { ok: false, message }
      } finally {
        this.savingProvider = false
      }
    },

    /**
     * 删除供应商：后端会级联删掉它的模型缓存，
     * 引用它的会话回退到默认供应商（所以删完要把这两个都重拉）。
     */
    async removeProvider(id: number): Promise<SaveResult> {
      this.deletingId = id
      try {
        await api.deleteProvider(id)
        delete this.fetchResults[id]
        delete this.testResults[id]
        await this.reload()
        toast.success('已删除供应商')
        return { ok: true, message: '' }
      } catch (err) {
        const message = toError(err).message
        toast.error(`删除失败：${message}`)
        return { ok: false, message }
      } finally {
        this.deletingId = null
      }
    },

    /* ---------------- 单个供应商的操作 ---------------- */

    /** 从上游拉模型列表；失败把后端 error.message 原样展示在该行下面 */
    async fetchModelsFromUpstream(id: number): Promise<void> {
      if (this.isFetching(id)) return
      this.fetchingIds = [...this.fetchingIds, id]
      delete this.fetchResults[id]
      try {
        const res = await api.fetchProviderModels(id)
        this.fetchResults[id] = {
          ok: true,
          total: res?.total ?? 0,
          added: res?.added ?? 0,
          message: '',
        }
        // 模型列表 + 各家的 model_count + 后端可能顺手设的默认模型
        await this.reload()
      } catch (err) {
        this.fetchResults[id] = { ok: false, total: 0, added: 0, message: toError(err).message }
      } finally {
        this.fetchingIds = this.fetchingIds.filter((x) => x !== id)
      }
    },

    /**
     * 测试连接。注意上游报错时接口也返回 HTTP 200 且 ok:false，
     * 这时 message 才是错误说明，要当错误展示。
     */
    async testConnection(id: number, modelId?: string): Promise<void> {
      if (this.isTesting(id)) return
      const model = modelId || this.modelsOf(id)[0]?.model_id || undefined
      this.testingIds = [...this.testingIds, id]
      delete this.testResults[id]
      try {
        this.testResults[id] = await api.testProvider(id, model)
      } catch (err) {
        this.testResults[id] = { ok: false, latency_ms: 0, message: toError(err).message }
      } finally {
        this.testingIds = this.testingIds.filter((x) => x !== id)
      }
    },

    /** 弹窗每次打开时清掉上一次的操作结果，避免看到过期的「连接正常」 */
    clearOpResults(): void {
      this.fetchResults = {}
      this.testResults = {}
    },

    /* ---------------- 默认值 ---------------- */

    /**
     * 保存「新建会话的默认值」。
     * 传空串 / 0 表示清空该项，省略表示不动（契约 §4）。
     */
    async saveDefaults(patch: DefaultsInput): Promise<SaveResult> {
      this.savingDefaults = true
      try {
        const res = await api.updateDefaults(patch)
        this.applySettingsResponse(res)
        return { ok: true, message: '' }
      } catch (err) {
        const message = toError(err).message
        toast.error(`保存默认值失败：${message}`)
        return { ok: false, message }
      } finally {
        this.savingDefaults = false
      }
    },

    /**
     * 默认值是否「自洽」：
     * - 默认模型必须真的属于默认供应商（否则就是发给别家的一个名字）；
     * - 默认思考强度必须在该模型声明的 efforts 里（空串 = 默认，永远合法）。
     * 不自洽时返回需要显式清空的字段（省略 = 不改），自洽返回 null。
     */
    defaultsConsistencyPatch(): DefaultsInput | null {
      const pid = this.defaultProviderId
      const modelId = this.defaults.model_id
      if (modelId && !this.modelIn(pid, modelId)) {
        // 模型不属于这家：清掉模型；顺手把已经没有依据的强度也清掉
        return this.defaults.effort ? { model_id: '', effort: '' } : { model_id: '' }
      }
      const model = this.modelIn(pid, modelId)
      if (this.defaults.effort && !(model?.efforts ?? []).includes(this.defaults.effort)) {
        return { effort: '' }
      }
      return null
    },

    /**
     * 换默认供应商。
     *
     * model_id 的处理（这里最容易出错的地方）：**只下发 provider_id，不下发 model_id**。
     * - 契约 §4 只承诺「省略 = 不改」，并没有承诺换供应商时会顺手修 model_id；
     *   所以不能假设后端一定会纠正，改完必须自己核对一遍响应；
     * - 反过来，如果前端显式下发 model_id: ""，就等于把「新供应商下该用哪个模型」
     *   这个只有后端知道的信息（哪家有哪些模型）自己拍死了。
     *
     * 于是流程是：PUT {provider_id} → 用返回的权威 defaults 核对自洽性 →
     * 不自洽再补一次 PUT 清空（模型不属于这家 → 清模型 + 清强度；强度不在模型的
     * efforts 里 → 只清强度）。这样既不猜后端的行为，也不会留下
     * 「provider_id=2 却挂着 1 号供应商的模型」这种发给别家的组合。
     */
    async setDefaultProvider(providerId: number): Promise<SaveResult> {
      if (providerId === this.defaults.provider_id) return { ok: true, message: '' }

      const res = await this.saveDefaults({ provider_id: providerId })
      if (!res.ok) return res

      // 先按本地模型缓存核对；发现不自洽时再拉一次模型（可能只是本地缓存旧了），
      // 复核后仍然不自洽才去清 —— 免得把后端刚挑好的默认模型误清掉。
      let fix = this.defaultsConsistencyPatch()
      if (fix) {
        await this.loadModels()
        fix = this.defaultsConsistencyPatch()
      }
      if (fix) {
        const fixed = await this.saveDefaults(fix)
        if (!fixed.ok) return fixed
      }
      toast.success(`默认供应商已切到 ${this.providerName(providerId)}`)
      return { ok: true, message: '' }
    },

    /**
     * 设定默认模型（连带把默认供应商也写清楚）。
     * 供应商与模型**永远一起下发**：契约里「只改 provider_id 会把 model_id 清空」
     * 那条路前端一次都不走，省掉一次「先清空再设回来」的中间态。
     */
    async chooseDefaultModel(providerId: number, modelId: string): Promise<SaveResult> {
      const model = this.modelIn(providerId, modelId)
      const patch: DefaultsInput = { provider_id: providerId, model_id: modelId }
      if (this.defaults.effort && !(model?.efforts ?? []).includes(this.defaults.effort)) patch.effort = ''
      const res = await this.saveDefaults(patch)
      if (res.ok) toast.success(`默认模型已设为 ${modelId}`)
      return res
    },

    setDefaultModel(modelId: string): Promise<SaveResult> {
      return this.chooseDefaultModel(this.defaultProviderId, modelId)
    },

    async setDefaultEffort(effort: string): Promise<SaveResult> {
      const res = await this.saveDefaults({ effort })
      if (res.ok) toast.success('默认思考强度已保存')
      return res
    },
  },
})
