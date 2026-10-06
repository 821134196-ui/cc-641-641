<script setup>
defineProps({ status: { type: String, default: '' } })
const labels = {
  draft: '草稿', locked: '已锁定', ended: '已结束', drawn: '已开奖',
  registered: '已报名', in_review: '复核中', eligible: '资格有效', ineligible: '资格失效',
  pending: '待处理', confirmed: '已确认', forfeited: '已弃奖',
  reviewing: '复核中', disqualified: '已取消', shipped: '已发货', delivered: '已签收',
  available: '在库', claimed: '已占用', released: '已释放', cancelled: '已取消',
}
</script>

<template>
  <span class="badge" :class="'b-' + status">{{ labels[status] || status }}</span>
</template>
