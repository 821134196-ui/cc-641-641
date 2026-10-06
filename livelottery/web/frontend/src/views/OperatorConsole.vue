<script setup>
import { ref, inject, onMounted, computed, watch } from 'vue'
import { api } from '../api.js'
import StatusBadge from '../components/StatusBadge.vue'

const toast = inject('toast')

const activities = ref([])
const currentId = ref(null)
const act = ref(null)
const prizes = ref([])
const regs = ref([])
const result = ref(null)
const ships = ref([])
const audit = ref([])
const ledger = ref([])
const items = ref([])

const loading = ref(false)

// 创建活动表单
const form = ref({
  name: '新品首发直播抽奖',
  draw_at: '',
  min_account_age_days: 7,
  need_bound_phone: true,
  winner_count: 5,
  address_deadline_hours: 24,
})
const prizeForm = ref({ name: '蓝牙耳机', stock: 5 })
const trackInput = ref({})
const shipTrack = ref('')
const receiptForm = ref({ receipt_no: 'RCPT-1', tracking_no: '', event_type: 'delivered' })
const reviewTarget = ref(null)
const reviewNote = ref('')

const rule = computed(() => {
  if (!act.value || !act.value.rule_json) return null
  try { return JSON.parse(act.value.rule_json) } catch { return null }
})

async function loadActivities() {
  activities.value = await api.listActivities()
  if (!currentId.value && activities.value.length) currentId.value = activities.value[0].id
}

async function refresh() {
  if (!currentId.value) return
  loading.value = true
  try {
    act.value = await api.getActivity(currentId.value)
    prizes.value = await api.prizes(currentId.value)
    regs.value = await api.registrations(currentId.value)
    ships.value = await api.shipments(currentId.value)
    audit.value = await api.audit(currentId.value)
    result.value = null
    try { result.value = await api.result(currentId.value) } catch { /* 未开奖 */ }
    if (prizes.value[0]) {
      ledger.value = await api.ledger(prizes.value[0].id)
      items.value = await api.items(prizes.value[0].id)
      receiptForm.value.tracking_no = ships.value[0]?.tracking_no || ''
    } else { ledger.value = []; items.value = [] }
  } catch (e) {
    toast(e.message, 'err')
  } finally {
    loading.value = false
  }
}

watch(currentId, refresh)

async function createActivity() {
  try {
    const body = { ...form.value }
    if (!body.draw_at) {
      body.draw_at = new Date(Date.now() + 3600 * 1000).toISOString().slice(0, 16)
    }
    const a = await api.createActivity(body)
    await loadActivities()
    currentId.value = a.id
    toast('活动已创建（草稿态，规则尚未锁定）', 'ok')
  } catch (e) { toast(e.message, 'err') }
}

async function addPrize() {
  try {
    await api.addPrize(currentId.value, prizeForm.value)
    toast('奖品与编号库存已登记', 'ok')
    refresh()
  } catch (e) { toast(e.message, 'err') }
}

async function doLock() {
  try { await api.lock(currentId.value); toast('参加条件、抽奖规则与库存已锁定并冻结，开放报名', 'ok'); refresh() }
  catch (e) { toast(e.message, 'err') }
}
async function doEnd() {
  try { await api.end(currentId.value); toast('直播已结束，停止报名', 'ok'); refresh() }
  catch (e) { toast(e.message, 'err') }
}
async function doDraw() {
  try {
    const r = await api.draw(currentId.value)
    toast(`正式名单已公布：${r.winners.length} 人（唯一，不可重抽）`, 'ok'); refresh()
  } catch (e) { toast(e.message, 'err') }
}
async function doExpire() {
  try { const r = await api.expire(currentId.value); toast(`已按锁定规则处理超期 ${r.expired} 人，弃奖不补抽`, 'ok'); refresh() }
  catch (e) { toast(e.message, 'err') }
}
async function doVerify() {
  try { const r = await api.verifyHash(currentId.value)
    toast(r.hash_matches ? '名单哈希校验通过，结果未被篡改' : '警告：名单哈希不一致，结果已被篡改！', r.hash_matches ? 'ok' : 'err')
  } catch (e) { toast(e.message, 'err') }
}

async function review(reg, confirm) {
  reviewTarget.value = reg
  reviewNote.value = reg.review_note || ''
  reviewConfirm.value = confirm
  reviewOpen.value = true
}
const reviewOpen = ref(false)
const reviewConfirm = ref(true)
async function submitReview() {
  try {
    await api.review(currentId.value, {
      account_id: reviewTarget.value.account_id, confirm: reviewConfirm.value, note: reviewNote.value,
    })
    reviewOpen.value = false
    toast(reviewConfirm.value ? '复核通过，资格恢复' : '已判定资格失效，名额不补抽', 'ok')
    refresh()
  } catch (e) { toast(e.message, 'err') }
}

async function doShip(w) {
  const tracking = shipTrack.value || ('TRACK-' + w.id + '-' + Math.floor(performance.now()))
  try {
    await api.ship(currentId.value, { winner_record_id: w.id, tracking_no: tracking })
    toast(`已按奖品编号出库发货，单号 ${tracking}`, 'ok'); shipTrack.value = ''; refresh()
  } catch (e) { toast(e.message, 'err') }
}

async function doReceipt() {
  try {
    const r = await api.receipt(receiptForm.value)
    toast(r.duplicate ? '重复回执，已幂等忽略（发货数量不变）' : '物流回执已处理', r.duplicate ? 'info' : 'ok')
    refresh()
  } catch (e) { toast(e.message, 'err') }
}

async function simDemo() {
  try { const d = await api.simDemo(); await loadActivities(); currentId.value = d.activity_id; toast('演示数据已就绪：已锁定、8人报名、开奖完成', 'ok') }
  catch (e) { toast(e.message, 'err') }
}

const stockStat = computed(() => {
  const m = {}
  for (const it of items.value) m[it.status] = (m[it.status] || 0) + 1
  return m
})

function fmt(t) { return t ? new Date(t).toLocaleString('zh-CN') : '—' }

onMounted(async () => { await loadActivities(); refresh() })
</script>

<template>
  <div class="card">
    <div class="row">
      <h2 style="margin:0">选择活动</h2>
      <select v-model.number="currentId" style="max-width:340px">
        <option v-for="a in activities" :key="a.id" :value="a.id">#{{ a.id }} {{ a.name }}</option>
      </select>
      <div class="spacer"></div>
      <button class="ghost" @click="refresh">刷新</button>
      <button class="ok" @click="simDemo">⚡ 一键生成演示活动</button>
    </div>
  </div>

  <!-- 创建活动 -->
  <div class="card">
    <h2>① 创建活动（草稿态配置，锁定后冻结）</h2>
    <div class="row">
      <div class="field"><label>活动名称</label><input v-model="form.name" /></div>
      <div class="field"><label>公示开奖时间</label><input v-model="form.draw_at" placeholder="留空=1小时后" /></div>
    </div>
    <div class="row">
      <div class="field"><label>账号注册满(天)</label><input type="number" v-model.number="form.min_account_age_days" /></div>
      <div class="field"><label>需绑定手机</label>
        <select v-model="form.need_bound_phone"><option :value="true">是</option><option :value="false">否</option></select>
      </div>
      <div class="field"><label>中奖人数</label><input type="number" v-model.number="form.winner_count" /></div>
      <div class="field"><label>填地址时限(小时)</label><input type="number" v-model.number="form.address_deadline_hours" /></div>
      <div class="field" style="display:flex;align-items:flex-end"><button @click="createActivity">创建活动</button></div>
    </div>
  </div>

  <template v-if="act">
    <!-- 活动与规则 -->
    <div class="card">
      <div class="row">
        <h2 style="margin:0">② 活动 #{{ act.id }}：{{ act.name }}</h2>
        <StatusBadge :status="act.status" />
        <div class="spacer"></div>
        <button v-if="act.status==='draft'" @click="doLock">🔒 锁定规则与库存</button>
        <button v-if="act.status==='locked'" class="warn" @click="doEnd">结束直播 / 停止报名</button>
        <button v-if="act.status==='ended'" class="ok" @click="doDraw">🎲 核验资格并开奖</button>
        <button class="ghost" @click="doVerify">校验名单哈希</button>
        <button class="ghost" @click="doExpire">处理超期未填地址</button>
      </div>
      <dl class="kv" style="margin-top:12px">
        <dt>公示开奖时间</dt><dd>{{ fmt(act.draw_at) }}</dd>
        <dt>锁定时间</dt><dd>{{ fmt(act.locked_at) }}</dd>
        <dt>开奖时间</dt><dd>{{ fmt(act.drawn_at) }}</dd>
      </dl>
      <h3>锁定的规则快照（锁定后只读）</h3>
      <div v-if="rule" class="kv">
        <dt>参加条件</dt><dd>账号注册满 {{ rule.min_account_age_days }} 天；需绑手机：{{ rule.need_bound_phone ? '是' : '否' }}</dd>
        <dt>中奖人数</dt><dd>{{ rule.winner_count }}</dd>
        <dt>填地址时限</dt><dd>{{ rule.address_deadline_hours }} 小时，超期处理：弃奖、释放库存、不补抽</dd>
        <dt>退货规则</dt><dd>订单退货 → 资格失效进入复核；{{ rule.return_invalidates_eligibility ? '已启用' : '未启用' }}</dd>
        <dt>开奖算法</dt><dd class="mono">{{ rule.algorithm }}</dd>
      </div>
      <p v-else class="muted">尚未锁定，无冻结快照。</p>
    </div>

    <!-- 奖品与库存 -->
    <div class="card">
      <h2>③ 奖品与编号库存</h2>
      <div v-if="act.status==='draft'" class="row">
        <div class="field"><label>奖品名称</label><input v-model="prizeForm.name" /></div>
        <div class="field"><label>库存件数（生成编号）</label><input type="number" v-model.number="prizeForm.stock" /></div>
        <div class="field" style="display:flex;align-items:flex-end"><button @click="addPrize">登记奖品</button></div>
      </div>
      <table v-if="prizes.length">
        <thead><tr><th>奖品</th><th>锁定总库存</th><th>在库</th><th>已占用</th><th>已发货</th><th>已释放</th></tr></thead>
        <tbody>
          <tr v-for="p in prizes" :key="p.id">
            <td>{{ p.name }}</td><td>{{ p.total_stock }}</td>
            <td>{{ stockStat.available || 0 }}</td>
            <td>{{ stockStat.claimed || 0 }}</td>
            <td>{{ stockStat.shipped || 0 }}</td>
            <td>{{ stockStat.released || 0 }}</td>
          </tr>
        </tbody>
      </table>
      <p v-else class="muted">暂无奖品。</p>

      <h3>库存流水（StockLedger，可审计）</h3>
      <div class="scroll">
        <table>
          <thead><tr><th>#</th><th>变动</th><th>变动后在库</th><th>原因</th><th>关联</th><th>时间</th></tr></thead>
          <tbody>
            <tr v-for="l in ledger" :key="l.id">
              <td class="mono">{{ l.id }}</td>
              <td :class="l.change<0?'bad-text':(l.change>0?'ok-text':'muted')">{{ l.change>0?'+':''}}{{ l.change }}</td>
              <td>{{ l.balance_after }}</td><td>{{ l.reason }}</td>
              <td class="mono">{{ l.ref_type }}:{{ l.ref_id }}</td><td class="muted">{{ fmt(l.created_at) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- 报名与复核 -->
    <div class="card">
      <h2>④ 报名名单与退货复核（{{ regs.length }} 人）</h2>
      <p class="muted" style="margin-top:0">同一账号只能占一个名额；订单退货进入复核，不删除名额。</p>
      <div class="scroll">
        <table>
          <thead><tr><th>账号</th><th>订单</th><th>资格状态</th><th>注册天数</th><th>绑手机</th><th>备注</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="r in regs" :key="r.id">
              <td class="mono">{{ r.account_id }}</td>
              <td class="mono">{{ r.order_no }}</td>
              <td><StatusBadge :status="r.status" /></td>
              <td>{{ r.account_age_days }}</td>
              <td>{{ r.phone_bound ? '是' : '否' }}</td>
              <td class="muted">{{ r.review_note }}</td>
              <td class="row tight">
                <button v-if="r.status==='in_review'" class="sm ok" @click="review(r,true)">通过</button>
                <button v-if="r.status==='in_review'" class="sm bad" @click="review(r,false)">判失效</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <p class="muted" style="margin-top:8px">💡 模拟退货事件可在「观众页面」用账号触发，或调用 <code>POST /api/activities/:id/return</code>。</p>
    </div>

    <!-- 唯一正式名单 -->
    <div class="card">
      <div class="row">
        <h2 style="margin:0">⑤ 唯一正式名单</h2>
        <span v-if="result" class="mono muted">批次 {{ result.batch.batch_code }}</span>
        <div class="spacer"></div>
        <span v-if="result" class="mono">hash: {{ result.batch.result_hash.slice(0,16) }}…</span>
      </div>
      <p v-if="!result" class="muted" style="margin:10px 0 0">尚未开奖；开奖后这里展示唯一正式名单，重复开奖会被系统拒绝。</p>
      <table v-if="result">
        <thead><tr><th>名次</th><th>账号</th><th>中奖状态</th><th>奖品编号</th><th>收货信息</th><th>发货</th></tr></thead>
        <tbody>
          <tr v-for="w in result.winners" :key="w.id">
            <td>{{ w.rank }}</td>
            <td class="mono">{{ w.account_id }}</td>
            <td><StatusBadge :status="w.status" /></td>
            <td class="mono">{{ w.prize_item_id ? ('#' + w.prize_item_id) : '—' }}</td>
            <td class="muted" style="max-width:220px">
              <template v-if="w.receiver">{{ w.receiver }} / {{ w.phone }} / {{ w.address }}</template>
              <template v-else>—</template>
            </td>
            <td>
              <button v-if="w.status==='confirmed'" class="sm" @click="doShip(w)">按编号发货</button>
              <span v-else-if="w.status==='shipped'||w.status==='delivered'" class="muted">已出库</span>
              <span v-else class="muted">—</span>
            </td>
          </tr>
        </tbody>
      </table>
      <div v-if="result && result.winners.some(w=>w.status==='confirmed')" class="row" style="margin-top:10px">
        <div class="field"><label>物流单号（留空自动生成）</label><input v-model="shipTrack" placeholder="TRACK-..." /></div>
        <span class="muted">↑ 对上面某位已确认中奖者点「按编号发货」</span>
      </div>
    </div>

    <!-- 发货与物流 -->
    <div class="card">
      <h2>⑥ 发货跟踪与物流回执（幂等）</h2>
      <table>
        <thead><tr><th>发货单</th><th>中奖记录</th><th>奖品编号</th><th>物流单号</th><th>状态</th><th>发货时间</th><th>签收时间</th></tr></thead>
        <tbody>
          <tr v-for="s in ships" :key="s.id">
            <td>#{{ s.id }}</td><td class="mono">win:{{ s.winner_record_id }}</td>
            <td class="mono">#{{ s.prize_item_id }}</td>
            <td class="mono">{{ s.tracking_no }}</td>
            <td><StatusBadge :status="s.status" /></td>
            <td class="muted">{{ fmt(s.shipped_at) }}</td><td class="muted">{{ fmt(s.delivered_at) }}</td>
          </tr>
        </tbody>
      </table>
      <div class="row" style="margin-top:12px">
        <div class="field"><label>回执号（重复提交将被忽略）</label><input v-model="receiptForm.receipt_no" /></div>
        <div class="field"><label>物流单号</label><input v-model="receiptForm.tracking_no" /></div>
        <div class="field"><label>事件</label>
          <select v-model="receiptForm.event_type"><option value="shipped">shipped</option><option value="delivered">delivered</option></select>
        </div>
        <div class="field" style="display:flex;align-items:flex-end"><button @click="doReceipt">发送物流回执</button></div>
      </div>
      <p class="muted">同一回执号重复投递不会改变发货数量；重复创建发货单会被唯一约束拒绝。</p>
    </div>

    <!-- 审计 -->
    <div class="card">
      <h2>⑦ 操作审计</h2>
      <div class="scroll">
        <table>
          <thead><tr><th>时间</th><th>操作者</th><th>动作</th><th>详情</th></tr></thead>
          <tbody>
            <tr v-for="l in audit.slice().reverse()" :key="l.id">
              <td class="muted">{{ fmt(l.created_at) }}</td><td class="mono">{{ l.actor }}</td>
              <td class="mono">{{ l.action }}</td><td>{{ l.detail }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </template>

  <!-- 复核弹窗 -->
  <div v-if="reviewOpen" style="position:fixed;inset:0;background:rgba(0,0,0,.5);display:flex;align-items:center;justify-content:center;z-index:50" @click.self="reviewOpen=false">
    <div class="card" style="width:420px;max-width:92vw">
      <h2>资格复核 · {{ reviewTarget?.account_id }}</h2>
      <p class="muted">{{ reviewTarget?.review_note }}</p>
      <label>复核备注</label><textarea v-model="reviewNote" rows="3"></textarea>
      <div class="row" style="margin-top:12px">
        <div class="spacer"></div>
        <button class="ghost" @click="reviewOpen=false">取消</button>
        <button :class="reviewConfirm?'ok':'bad'" @click="submitReview">{{ reviewConfirm ? '确认通过' : '判定失效' }}</button>
      </div>
    </div>
  </div>
</template>
