<script setup>
import { ref, inject, computed, onMounted } from 'vue'
import { api } from '../api.js'
import StatusBadge from '../components/StatusBadge.vue'

const toast = inject('toast')
const activities = ref([])
const actId = ref(null)
const account = ref('viewer-01')
const st = ref(null)

const regForm = ref({ order_no: 'ORD-viewer-01', account_age_days: 30, phone_bound: true })
const addr = ref({ receiver: '', phone: '', address: '' })

const rule = computed(() => {
  try { return st.value?.activity ? JSON.parse(st.value.activity.rule_json) : null } catch { return null }
})
const winner = computed(() => st.value?.winner || null)

async function loadActivities() {
  activities.value = await api.listActivities()
  if (!actId.value && activities.value[0]) actId.value = activities.value[0].id
}

async function load() {
  if (!actId.value || !account.value) return
  try { st.value = await api.viewer(actId.value, account.value.trim()) }
  catch (e) { st.value = null; toast(e.message, 'err') }
}

async function register() {
  try {
    await api.register(actId.value, { account_id: account.value.trim(), ...regForm.value })
    toast('报名成功，名额已占用（重复报名会被拒绝）', 'ok'); load()
  } catch (e) { toast(e.message, 'err') }
}
async function confirmWin() {
  try {
    await api.confirmWin(actId.value, account.value.trim(), addr.value)
    toast('中奖已确认，奖品编号已原子锁定并扣减库存', 'ok'); load()
  } catch (e) { toast(e.message, 'err') }
}
async function triggerReturn() {
  try {
    await api.returnOrder(actId.value, { account_id: account.value.trim(), order_no: regForm.value.order_no })
    toast('已模拟订单退货：资格进入复核', 'info'); load()
  } catch (e) { toast(e.message, 'err') }
}

function fmt(t) { return t ? new Date(t).toLocaleString('zh-CN') : '—' }

onMounted(async () => { await loadActivities() })
</script>

<template>
  <div class="card">
    <div class="row">
      <div class="field">
        <label>选择活动</label>
        <select v-model.number="actId" @change="load">
          <option v-for="a in activities" :key="a.id" :value="a.id">#{{ a.id }} {{ a.name }}</option>
        </select>
      </div>
      <div class="field">
        <label>我的账号</label>
        <input v-model="account" @keyup.enter="load" placeholder="例如 viewer-01" />
      </div>
      <div class="field" style="display:flex;align-items:flex-end"><button @click="load">查询我的状态</button></div>
    </div>
  </div>

  <template v-if="st && st.activity">
    <!-- 公示信息 -->
    <div class="card">
      <div class="row">
        <h2 style="margin:0">{{ st.activity.name }}</h2>
        <StatusBadge :status="st.activity.status" />
      </div>
      <dl class="kv" style="margin-top:12px">
        <dt>开奖时间</dt><dd class="ok-text">🎯 {{ fmt(st.activity.draw_at) }}</dd>
      </dl>
      <div v-if="rule">
        <h3>参加条件与抽奖规则（活动开始前已锁定）</h3>
        <ul class="muted" style="margin:0;padding-left:18px;line-height:1.9">
          <li>账号注册满 {{ rule.min_account_age_days }} 天{{ rule.need_bound_phone ? '，且需绑定手机' : '' }}</li>
          <li>正式中奖名额 {{ rule.winner_count }} 名，开奖算法 <span class="mono">{{ rule.algorithm }}</span></li>
          <li>中奖后需在 {{ rule.address_deadline_hours }} 小时内填写地址，超期按规则<strong>弃奖且不补抽</strong></li>
          <li>关联订单退货将导致资格失效并进入人工复核</li>
        </ul>
      </div>
    </div>

    <!-- 报名 -->
    <div class="card">
      <h2>我的报名</h2>
      <template v-if="st.registration">
        <div class="row">
          <span class="mono">{{ st.registration.account_id }}</span>
          <StatusBadge :status="st.registration.status" />
          <span class="muted">订单 {{ st.registration.order_no }}</span>
          <div class="spacer"></div>
          <button class="ghost sm" @click="triggerReturn"
            :disabled="['ineligible','in_review'].includes(st.registration.status)">模拟该订单退货</button>
        </div>
        <p v-if="st.registration.review_note" class="warn-text" style="margin:8px 0 0">{{ st.registration.review_note }}</p>
      </template>
      <template v-else>
        <p v-if="st.activity.status==='locked'" class="muted">活动报名开放中，填写模拟账号资料后报名（同一账号不能重复占名额）：</p>
        <p v-else class="muted">当前不在报名时段（活动 {{ st.activity.status }}）。</p>
        <div v-if="st.activity.status==='locked'" class="row">
          <div class="field"><label>关联订单号</label><input v-model="regForm.order_no" /></div>
          <div class="field"><label>账号注册天数</label><input type="number" v-model.number="regForm.account_age_days" /></div>
          <div class="field"><label>已绑手机</label>
            <select v-model="regForm.phone_bound"><option :value="true">是</option><option :value="false">否</option></select>
          </div>
          <div class="field" style="display:flex;align-items:flex-end"><button @click="register">立即报名</button></div>
        </div>
      </template>
    </div>

    <!-- 开奖结果 -->
    <div class="card">
      <h2>开奖结果</h2>
      <p v-if="!st.drawn" class="muted">⏳ 尚未开奖，请于公示开奖时间后回来查看。正式名单只有一份，公布后不会重新抽取。</p>
      <template v-else>
        <p class="mono muted" style="margin:0 0 10px">名单哈希（可用于核验未被篡改）：{{ st.result_hash }}</p>
        <template v-if="winner">
          <div class="row">
            <span class="ok-text" style="font-size:16px">🎉 恭喜中奖！名次 #{{ winner.rank }}</span>
            <StatusBadge :status="winner.status" />
          </div>

          <div v-if="winner.status==='pending'" class="card" style="background:var(--panel-2);margin-top:12px">
            <h3>请填写收货地址（超时按锁定规则弃奖）</h3>
            <div class="row">
              <div class="field"><label>收货人</label><input v-model="addr.receiver" /></div>
              <div class="field"><label>手机号</label><input v-model="addr.phone" /></div>
            </div>
            <label>详细地址</label><textarea v-model="addr.address" rows="2"></textarea>
            <div class="row" style="margin-top:10px">
              <button class="ok" @click="confirmWin">确认中奖并提交地址（同时锁定奖品）</button>
              <span class="muted">提交成功即与库存扣减原子完成，不会出现“中奖却无货”</span>
            </div>
          </div>

          <div v-else-if="winner.status==='confirmed'" style="margin-top:10px">
            <p class="muted" style="margin:0">✅ 已确认，奖品编号 <span class="mono">#{{ winner.prize_item_id }}</span> 已锁定，等待运营发货。</p>
          </div>
          <div v-else-if="winner.status==='reviewing'" style="margin-top:10px">
            <p class="warn-text" style="margin:0">⚠️ 检测到关联订单退货，中奖资格复核中，发货暂缓。</p>
          </div>
          <div v-else-if="winner.status==='shipped'||winner.status==='delivered'" style="margin-top:10px">
            <p style="margin:0 0 6px">📦 奖品已按编号 <span class="mono">#{{ winner.prize_item_id }}</span> 发出</p>
            <p v-if="st.shipment" class="muted" style="margin:0">
              物流单号 <span class="mono">{{ st.shipment.tracking_no }}</span>，状态 <StatusBadge :status="st.shipment.status" />
            </p>
          </div>
          <div v-else-if="winner.status==='forfeited'" style="margin-top:10px">
            <p class="bad-text" style="margin:0">⌛ 超过锁定规则规定的地址时限，已按规则判弃奖，名额不补抽。</p>
          </div>
          <div v-else-if="winner.status==='disqualified'" style="margin-top:10px">
            <p class="bad-text" style="margin:0">❌ 复核后资格失效，中奖取消（正式名单本身不改变、不补抽）。</p>
          </div>
        </template>
        <p v-else class="muted" style="margin:8px 0 0">很遗憾，您不在本次正式名单中。名单已公布且唯一，不会重新开奖。</p>
      </template>
    </div>
  </template>
</template>
