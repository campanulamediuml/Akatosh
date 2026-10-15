<script setup lang="ts">
/**
 * 访问令牌输入弹窗。
 *
 * 什么时候出现：后端用 `-token` 启动，而这个浏览器还没有（或给错了）令牌，
 * 于是任何 `/api/*` 请求都返回 401。client.ts 在解析出 401 时会调
 * `notifyUnauthorized`，App.vue 收到就把这个弹窗打开。
 *
 * 为什么还需要手输：手机上的正常路径是打开 `http://<IP>:28888/?token=xxx`，
 * 服务端校验通过后下发一年期 cookie 并 302 抹掉地址栏 —— 那条路径根本不会走到这里。
 * 这里兜的是两种情况：
 *   1. 用户直接把裸地址加进了主屏幕/书签，没带 ?token=
 *   2. 浏览器禁用了 cookie（隐私模式、设置里关掉了），cookie 那条路走不通
 *
 * 注意保存时会**同时**写 localStorage（API 请求的 X-Auth-Token 头）和 cookie
 * （`<img src>` 这类加不了 header 的请求只能靠它，不种的话图片全部 401 红叹号）。
 */
import { computed, nextTick, ref, watch } from 'vue'
import { getAuthToken, setAuthToken } from '@/api/client'

const props = defineProps<{ open: boolean; hint?: string }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const draft = ref('')
const input = ref<HTMLInputElement | null>(null)
const error = ref('')

const current = computed(() => getAuthToken())
const canSave = computed(() => draft.value.trim().length > 0)

watch(
  () => props.open,
  (open) => {
    if (!open) return
    error.value = ''
    // 不回填已存的令牌：那是敏感值，没必要再摆到屏幕上
    draft.value = ''
    void nextTick(() => input.value?.focus())
  },
)

function save(): void {
  const v = draft.value.trim()
  if (!v) {
    error.value = '请填写访问令牌'
    return
  }
  // setAuthToken 会同时种 cookie（<img> 这类加不了 header 的请求靠它）
  setAuthToken(v)
  // 直接重载：这样所有 store、SSE 长连接、已发出的请求都会带上新令牌重新开始，
  // 比在运行中逐个补丁可靠得多，而且这里没有值得保留的未保存状态。
  location.reload()
}

function clearToken(): void {
  setAuthToken('')
  location.reload()
}
</script>

<template>
  <div v-if="open" class="modal" @click.self="emit('close')">
    <div class="modal__panel modal__panel--narrow" role="dialog" aria-modal="true" aria-label="访问令牌">
      <header class="modal__head">
        <span class="modal__title">需要访问令牌</span>
      </header>

      <div class="modal__body">
        <p class="token__lead">
          这个服务需要访问令牌才能使用（每个用户一个，数据互相隔离），当前请求被拒绝了（HTTP 401）。
        </p>

        <p v-if="hint" class="token__note">{{ hint }}</p>

        <label class="token__label" for="token-input">访问令牌</label>
        <input
          id="token-input"
          ref="input"
          v-model="draft"
          class="input token__input"
          type="text"
          autocomplete="off"
          spellcheck="false"
          autocapitalize="off"
          autocorrect="off"
          data-lpignore="true"
          data-1p-ignore="true"
          data-bwignore="true"
          placeholder="粘贴令牌"
          @keydown.enter.prevent="save"
        />
        <p v-if="error" class="token__error">{{ error }}</p>

        <p v-if="current" class="token__note token__note--muted">
          当前已经存了一个令牌，但后端不认（可能后端换过令牌了）。
        </p>

        <details class="token__help">
          <summary>令牌从哪儿拿？</summary>
          <p>
            找这个服务的管理员要。管理员在「设置 → 用户管理」里创建用户，每个用户一个令牌；
            直接打开管理员发的「登录链接」最省事 —— 服务端会顺手把令牌换成 cookie 并把它从地址栏抹掉，
            之后从主屏幕启动也不用再输。
          </p>
          <p>
            如果浏览器禁用了 cookie，接口能通但图片可能显示不出来 —— 令牌存下来之后走的是请求头，
            而图片是浏览器直接发起的请求，带不了自定义头。
          </p>
        </details>
      </div>

      <footer class="modal__foot">
        <button v-if="current" type="button" class="btn btn--sm" @click="clearToken">清除已存的令牌</button>
        <span class="modal__foot-spacer" />
        <button type="button" class="btn btn--sm" @click="emit('close')">稍后</button>
        <button type="button" class="btn btn--sm btn--primary" :disabled="!canSave" @click="save">
          保存并重新加载
        </button>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.modal__panel--narrow {
  max-width: 460px;
}

.token__lead {
  margin: 0 0 var(--sp-3);
  font-size: var(--fs-sm);
  line-height: 1.7;
  color: var(--text-muted);
}

.token__label {
  display: block;
  margin-bottom: var(--sp-1);
  font-size: var(--fs-sm);
  color: var(--text-muted);
}

.token__input {
  width: 100%;
  font-family: var(--font-mono);
  /* 16px：低于这个值 iOS 聚焦时会缩放整页 */
  font-size: 16px;
}

.token__error {
  margin: var(--sp-2) 0 0;
  font-size: var(--fs-sm);
  color: var(--danger);
}

.token__note {
  margin: var(--sp-2) 0 0;
  font-size: var(--fs-sm);
  line-height: 1.7;
  color: var(--text-muted);
}

.token__note--muted {
  color: var(--text-faint);
}

.token__help {
  margin-top: var(--sp-4);
  font-size: var(--fs-sm);
  color: var(--text-muted);
}

.token__help summary {
  cursor: pointer;
  color: var(--text);
}

.token__help p {
  margin: var(--sp-2) 0 0;
  line-height: 1.7;
}

code {
  padding: 0 4px;
  border-radius: var(--radius-sm);
  background: var(--bg-hover);
  font-family: var(--font-mono);
  font-size: 0.92em;
}
</style>
