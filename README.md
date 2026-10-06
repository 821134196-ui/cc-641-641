# 电商直播抽奖资格与奖品发货系统

从直播报名到奖品发放的全链路可查系统：**规则先锁定 → 再开放报名 → 结束核验资格 → 唯一正式开奖 → 中奖确认与库存原子扣减 → 填址 → 按奖品编号发货 → 物流回执跟踪 → 退货复核**。

技术栈：Go + Gin + GORM + SQLite（pure-Go 驱动，无 CGO）+ Vue 3（离线打包进二进制）。直播观众流量与物流回执均在本地模拟。

## 一条命令启动

```bash
./run.sh            # 首次自动编译并启动 http://localhost:8080
./run.sh --reset    # 清空演示数据重新开始
```

启动后内置一个「秋季新品直播专场」演示活动（草稿态，15 个模拟账号）和两个后台模拟器：
- **直播流量**：活动处于报名中时，模拟器每 400ms 替一个观众报名，并周期性穿插重复报名（被唯一索引拒绝）。
- **物流回执**：发货后自动推送妥投回执，并周期性重发历史回执号（用于现场观察幂等）。

页面三个视图：👤 观众中心（活动规则/开奖时间/正式名单/确认/填址）、🛠 运营后台（阶段推进、库存、按编号发货、退货、事件流）、📜 审计中心（种子重放、规则哈希、库存对账、复核单、审计日志）。

## 关键业务规则及其落地方式

| 规则 | 实现保证 |
|---|---|
| 先锁定参加条件/抽奖规则/奖品库存，再开放报名 | 活动状态机 `draft→locked→open→closed→drawn`；锁定后生成 `RuleSnapshot`（含规则哈希、奖品单件清单哈希），代码无任何修改入口 |
| 同一账号不能重复占名额 | `registrations(campaign_id,account_id)` 唯一索引兜底 + 并发报名实测 10 并发仅 1 成功 |
| 运营重复操作不能生成第二份正式开奖结果 | `draw_batches.campaign_id` 唯一索引；重复/并发开奖返回「正式名单已存在，禁止再次开奖」；开奖后退货、超期、重放都不会重抽 |
| 开奖结果可审计 | 开奖种子（32 字节随机）持久化并公布；带种子 Fisher-Yates 可逐名次重放；合格名单 Merkle 根哈希；规则哈希 |
| 中奖确认与库存扣减必须同时成功 | 单 IMMEDIATE 事务：`UPDATE prizes SET remaining=remaining-1 WHERE remaining>0` 影响 0 行则整体回滚；同时条件占用一件最小编号实物，绝不可能"确认了却无货" |
| 超期未填地址按锁定规则处理 | 快照中的 `address_limit_minutes`；后台每 10 秒巡检 + 运营台可模拟时间快进；作废名额对应奖品**永久退出可发库存，不补抽** |
| 订单退货致资格失效进入复核 | `ReviewCase` 按阶段处置：开奖前→核验排除；待确认→作废退出名额；已确认/已填址→奖品退出；在途→挂起人工处理；已妥投→仅记录。任何阶段都不重新开奖 |
| 重复物流回执不能改变已确认发货数量 | `logistics_receipts.receipt_no` 唯一索引（同号重发只返回 duplicate 不落库）；发货单/条目/单件状态更新全部带当前状态条件，跨回执号重复妥投也不二次推进 |
| 按奖品编号发货 | 每件奖品有全局唯一 `serial_no`；`shipments.entry_id` 与 `prize_unit_id` 均唯一，一条中奖记录、一件实物最多一张运单 |
| 库存可审计 | `inventory_transactions` 流水（lock/allocate/retire）；审计中心对账：流水轧差=余量、在库单件数=余量、运单数=已发+妥投 |

SQLite 层使用 WAL + `_txlock=immediate` + busy_timeout，并发写事务在数据库层串行排队；所有一致性最终都由**条件 UPDATE 与唯一索引**兜底，而不是仅靠应用层判断。

## 目录结构

```
cmd/server/main.go        入口：迁移、静态资源 embed、模拟器协程、超期巡检
internal/models/          GORM 模型（含全部唯一索引）
internal/lottery/         可审计开奖：种子、Fisher-Yates、Merkle 根
internal/store/           SQLite 打开配置与事务封装
internal/service/         业务规则（事务边界都在这一层）
internal/sim/             本地直播流量 / 物流事件模拟
internal/api/             Gin REST 接口
web/                      Vue3 单页（vendor/ 内含离线 Vue）
```

## 自动验证

```bash
go test ./internal/... -race
```

测试覆盖（`internal/service/service_test.go`）：

1. **`TestConcurrentConfirmNoOversell`** — 10 个名额 ×20 个并发确认：恰好 10 成功 10 拒绝，余量恰好 0，无编号被两人占用，流水轧平。
2. **`TestConcurrentConfirmWithScarceStock`** — 账面只剩 3 件时 10 并发确认：恰好 3 成功 7 回滚，库存不为负。
3. **`TestOfficialDrawIsUnique`** — 串行 + 8 并发重复开奖全部被拒，`draw_batches` 恒为 1，种子重放一致。
4. **`TestDuplicateRegistrationRejected`** — 同账号 10 并发报名仅 1 条记录。
5. **`TestDuplicateLogisticsReceiptIdempotent`** — 同回执号 6 并发仅 1 次落库、1 张运单、发货数量不放大。
6. **`TestForfeitFollowsLockedRulesNoRedraw`** — 时限内不处理、超期按锁定规则放弃且收回名额、不允许重抽。
7. **`TestReturnOrderAfterDrawReview`** — 开奖后退货分阶段处置，名单保留、库存账实一致。
8. **`TestEndToEndInventoryConsistency`** — 全链路后库存对账全部为真。

> 说明：中奖名额与奖品一一对应（创建活动即要求总库存=中奖人数）。名额因超期/退货作废时，该件奖品标记 `recovered` 永久退出可发库存且不补抽——因此"剩余可发"只代表还能发给当前正式名单中待处理中奖者的数量，对账口径为：`期初 + 流水变动 = remaining`，`在库单件数 = remaining`。
