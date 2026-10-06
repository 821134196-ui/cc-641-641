<script setup>
import { ref, provide } from 'vue'
import OperatorConsole from './views/OperatorConsole.vue'
import ViewerPage from './views/ViewerPage.vue'

const tab = ref('operator')
const toast = ref(null)
let timer = null

function showToast(msg, kind = 'info') {
  toast.value = { msg, kind }
  clearTimeout(timer)
  timer = setTimeout(() => (toast.value = null), 3200)
}
provide('toast', showToast)
</script>

<template>
  <h1>🎁 电商直播抽奖 · 资格与奖品发货系统</h1>
  <p class="sub">
    规则先锁定后报名 · 唯一正式名单不可重抽 · 中奖确认与库存扣减原子提交 · 物流回执幂等
  </p>

  <div class="tabs">
    <div class="tab" :class="{ active: tab === 'operator' }" @click="tab = 'operator'">运营控制台</div>
    <div class="tab" :class="{ active: tab === 'viewer' }" @click="tab = 'viewer'">观众页面</div>
  </div>

  <OperatorConsole v-if="tab === 'operator'" />
  <ViewerPage v-else />

  <transition>
    <div v-if="toast" class="toast" :class="toast.kind">{{ toast.msg }}</div>
  </transition>
</template>
