# 电商直播抽奖资格与奖品发货系统

直播活动从**报名 → 资格核验 → 唯一开奖 → 中奖确认 → 奖品发货 → 物流跟踪**的全流程系统。
所有规则、名单、库存流水落 SQLite，可审计；直播与物流事件在本地模拟，无外部依赖。

技术栈：**Go + Gin + GORM + SQLite（modernc 纯驱动，免 CGO）+ Vue 3 + Vite**。
前端构建后通过 `go:embed` 打进单个二进制，一条命令启动。

## 一条命令启动

```bash
./run.sh
# 打开 http://localhost:8080
```

脚本会自动：安装/构建前端 → 编译 Go → 启动服务（默认 `:8080`，库文件 `data/lottery.db`）。
可用环境变量覆盖：`ADDR=:9000 DB_PATH=/tmp/x.db ./run.sh`，已装依赖时 `SKIP_FRONTEND=1` 跳过前端构建。

页面内置「⚡ 一键生成演示活动」按钮，可立即走通：锁定 → 8 人报名 → 开奖的完整链路。

## 一条命令验证（含并发）

```bash
./verify.sh
```

执行：`go test -race` 全部单元/集成测试 + 启动真实 HTTP 服务跑 38 项端到端断言。
重点证明你要求的三件事：

- **并发确认不会超扣库存**：5 个中奖者 × 各 8 次并发提交（40 个事务），最终占用实物恰好 5 件、在库 0、每个奖品编号只绑定一人、扣减流水恰好 5 条；
- **正式名单唯一**：16 个并发开奖请求只有 1 个成功，`draw_batches.activity_id` 唯一索引从数据库层拒绝第二份名单，之后串行重抽同样被拒；
- **重复回执不会重复发货**：同一回执号并发投递 12 次只登记 1 条，发货单始终 1 张、状态不回退；重复创建发货单也被唯一约束拒绝。

## 业务规则（锁定即冻结）

状态机（单向不可逆）：

```
draft ──锁定──▶ locked ──结束直播──▶ ended ──开奖──▶ drawn
                 │                    │
              开放报名             核验资格
```

1. **活动开始前锁定**参加条件（账号注册天数、是否绑手机）、抽奖规则（中奖人数、开奖算法）、奖品库存（每件生成唯一编号）。锁定后规则快照 `RuleJSON` 只读、库存不可增。
2. **同一账号不能重复占名额**：`(activity_id, account_id)` 唯一索引，并发报名只有一次成功。
3. **结束后核验资格开奖**：仅 `registered/eligible` 进入候选；`in_review/ineligible` 剔除。开奖结果为**唯一正式名单**，公布后不可重抽、不补抽。
4. 开奖用确定性算法：`seed = sha256(活动ID | 锁定时刻 | 有序合格账号)`，固定种子洗牌；名单附 `result_hash`（有序账号 sha256），任何篡改可被 `/verify-hash` 发现。
5. **中奖确认与库存扣减原子提交**（同一 immediate 事务）：
   - 取一件 `available` 编号并 `UPDATE ... SET claimed WHERE status='available'`，`RowsAffected` 必须为 1，否则整体回滚——绝不会“确认了中奖却无货”；
   - `prize_items.winner_record_id` 与 `winner_records.prize_item_id` 双唯一约束兜底。
6. **超期未填地址**：以锁定快照里的时限判定，判 `forfeited` 弃奖、释放名额、**不补抽**。
7. **订单退货 → 复核**：报名进入 `in_review`，已中奖则中奖记录进入 `reviewing` 冻结发货；运营判定失效则取消中奖、释放已占编号（不补抽），判定通过则恢复。正式名单本身永不改变。
8. **按奖品编号发货**：一个中奖记录至多一张发货单、一个编号至多出库一次。
9. **物流回执幂等**：回执号唯一，重复回执直接忽略；状态机只前进（shipped→delivered），不改变已确认的发货数量。

## 防超扣 / 幂等的数据库基础

`internal/store/db.go` 打开 SQLite 时设置：

- `journal_mode=WAL`：读写不互斥；
- `busy_timeout=8000`：锁竞争等待而非报 busy；
- `_txlock=immediate`：写事务一开始即取写锁，把“读改写”串行化，**从根上杜绝超扣**；
- 外键开启。关键唯一性：
  - `registrations(activity_id, account_id)`
  - `draw_batches.activity_id`、`draw_batches.batch_code`
  - `prize_items(serial_no per prize)`、`prize_items.winner_record_id`
  - `winner_records(draw_batch_id, account_id)`、`winner_records.prize_item_id`
  - `shipments.winner_record_id / prize_item_id / tracking_no`
  - `logistics_receipts.receipt_no`

## 数据模型（GORM，自动迁移）

| 表 | 作用 |
|---|---|
| `activities` | 活动、状态、公示开奖时间、锁定规则快照 |
| `prizes` / `prize_items` | 奖品 / 每件唯一编号库存（available→claimed→shipped/released） |
| `registrations` | 报名名单与资格状态 |
| `draw_batches` / `winner_records` | 开奖批次（唯一）/ 正式名单行 |
| `shipments` / `logistics_receipts` | 发货单 / 幂等物流回执 |
| `stock_ledgers` | 奖品库存流水（变动 + 变动后余额） |
| `audit_logs` | 全操作审计 |

## HTTP API（节选）

```
POST /api/activities                 建活动（草稿）
POST /api/activities/:id/prizes      登记奖品与编号库存（仅草稿）
POST /api/activities/:id/lock        锁定规则与库存
POST /api/activities/:id/register    观众报名
POST /api/activities/:id/return      模拟订单退货 → 复核
POST /api/activities/:id/review      运营复核结论
POST /api/activities/:id/end         结束直播
POST /api/activities/:id/draw        开奖（唯一正式名单）
GET  /api/activities/:id/result      查看正式名单
POST /api/activities/:id/verify-hash 名单哈希校验
POST /api/activities/:id/winners/:account/confirm  中奖确认（原子扣减）
POST /api/activities/:id/expire      批量处理超期未填地址
POST /api/activities/:id/ship        按编号发货
POST /api/logistics/receipt          物流回执（幂等）
GET  /api/prizes/:pid/ledger         库存流水
POST /api/sim/demo                   一键生成演示活动
```

## 目录结构

```
livelottery/
├── run.sh / verify.sh            一条命令启动 / 验证
├── cmd/server/main.go            入口（embed 静态资源 + SPA 回退）
├── internal/
│   ├── models/    数据模型与状态常量
│   ├── store/     SQLite 连接（WAL + immediate）
│   ├── service/   核心业务（规则锁定/报名/开奖/确认扣减/发货/复核/幂等）
│   └── api/       Gin handler
├── tests/integration/            穿过真实 HTTP 路由的并发集成测试
├── scripts/verify_e2e.py         端到端断言脚本
└── web/
    ├── static/                   go:embed 的前端产物（含占位 index.html）
    └── frontend/                 Vue 3 + Vite 源码（运营台 / 观众页）
```
