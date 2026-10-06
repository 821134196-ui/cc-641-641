package service_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"lottery/internal/models"
	"lottery/internal/service"
	"lottery/internal/sim"
	"lottery/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(models.AllModels()...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// setupWorld 创建活动、模拟观众与订单，并完成 锁定→报名→核验 全流程。
// 返回 service、campaignID、合格账号ID（按ID升序）。
func setupWorld(t *testing.T, winCount, accounts, ordersEach, minOrders int) (*service.Service, uint, []uint) {
	t.Helper()
	db := testDB(t)
	svc := service.New(db)
	simr := sim.New(svc)
	cid, err := svc.CreateCampaign("并发测试专场", minOrders, winCount, 30,
		[]service.PrizeSpec{{Code: "A", Name: "测试奖品", Stock: winCount}},
		time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	if err := simr.SeedWorld(cid, accounts, ordersEach); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.LockCampaign(cid); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := svc.OpenRegistration(cid); err != nil {
		t.Fatalf("open: %v", err)
	}
	var all []models.Account
	db.Order("id ASC").Find(&all)
	for _, a := range all {
		if _, err := svc.Register(cid, a.ID); err != nil {
			t.Fatalf("register %d: %v", a.ID, err)
		}
	}
	q, dq, err := svc.CloseAndVerify(cid)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if int(q) < winCount {
		t.Fatalf("合格人数 %d 小于中奖人数 %d", q, winCount)
	}
	var regs []models.Registration
	db.Where("campaign_id = ? AND qualified = ?", cid, true).Order("account_id ASC").Find(&regs)
	ids := make([]uint, 0, len(regs))
	for _, r := range regs {
		ids = append(ids, r.AccountID)
	}
	t.Logf("setup: 合格=%d 不合格=%d 中奖名额=%d", q, dq, winCount)
	return svc, cid, ids
}

func drawEntries(t *testing.T, svc *service.Service, cid uint) []models.DrawEntry {
	t.Helper()
	var batch models.DrawBatch
	if err := svc.DB().Where("campaign_id = ?", cid).First(&batch).Error; err != nil {
		t.Fatalf("load batch: %v", err)
	}
	var es []models.DrawEntry
	svc.DB().Where("batch_id = ?", batch.ID).Order("rank ASC").Find(&es)
	return es
}

//  1. 并发确认不得超扣库存：10 个名额，20 个并发确认（每个条目重复一次），
//     必须恰好 10 个成功、剩余库存恰好为 0、每个奖品编号最多被一人占用。
func TestConcurrentConfirmNoOversell(t *testing.T) {
	const N = 10
	svc, cid, _ := setupWorld(t, N, 20, 2, 1)
	batch, err := svc.Draw(cid)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	entries := drawEntries(t, svc, cid)
	if len(entries) != N {
		t.Fatalf("中奖条目数=%d, 期望 %d", len(entries), N)
	}

	// 每个条目发起两次确认，共 2*N 个并发请求（模拟重复点击/多端同时确认）
	var wg sync.WaitGroup
	var mu sync.Mutex
	success, rejected := 0, 0
	start := make(chan struct{})
	for _, e := range entries {
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(eid uint) {
				defer wg.Done()
				<-start
				_, err := svc.Confirm(eid)
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					success++
				} else {
					rejected++
				}
			}(e.ID)
		}
	}
	close(start)
	wg.Wait()

	if success != N {
		t.Errorf("成功确认数=%d，期望恰好 %d", success, N)
	}
	if rejected != N {
		t.Errorf("被拒绝数=%d，期望恰好 %d（重复确认必须失败）", rejected, N)
	}
	var prize models.Prize
	svc.DB().Where("campaign_id = ?", cid).First(&prize)
	if prize.Remaining != 0 {
		t.Errorf("超扣或漏扣：剩余库存=%d，期望 0", prize.Remaining)
	}
	// 占用的单件数必须恰好 N，且没有编号被两人占用
	var allocated int64
	svc.DB().Model(&models.PrizeUnit{}).
		Where("prize_id = ? AND status = ?", prize.ID, models.UnitAllocated).Count(&allocated)
	if int(allocated) != N {
		t.Errorf("已占用奖品单件=%d，期望 %d", allocated, N)
	}
	var dupWinner int64
	svc.DB().Model(&models.PrizeUnit{}).Where("prize_id = ?", prize.ID).
		Select("COUNT(*) - COUNT(DISTINCT winner_id)").Where("winner_id IS NOT NULL").Scan(&dupWinner)
	if dupWinner != 0 {
		t.Errorf("存在奖品编号被重复占用 dup=%d", dupWinner)
	}
	// 流水必须能轧平
	var sum int64
	svc.DB().Model(&models.InventoryTransaction{}).Where("prize_id = ?", prize.ID).
		Select("COALESCE(SUM(delta),0)").Scan(&sum)
	if sum != int64(prize.Remaining) {
		t.Errorf("库存流水轧差=%d，与剩余 %d 不一致", sum, prize.Remaining)
	}
	t.Logf("批次 %d: %d 成功 / %d 拒绝，库存余量 %d，无超扣", batch.ID, success, rejected, prize.Remaining)
}

// 1b) 极端情况：账面上剩余库存比中奖条目少（3 件库存对 10 条中奖），
//
//	并发确认只能成功 3 个，其余必须收到“库存不足整体回滚”。
func TestConcurrentConfirmWithScarceStock(t *testing.T) {
	const N = 10
	svc, cid, _ := setupWorld(t, N, 20, 2, 1)
	if _, err := svc.Draw(cid); err != nil {
		t.Fatalf("draw: %v", err)
	}
	entries := drawEntries(t, svc, cid)

	// 人为把可发库存压到 3（模拟账实紧张），单件数不动，验证条件扣减是最终闸门
	svc.DB().Model(&models.Prize{}).Where("campaign_id = ?", cid).Update("remaining", 3)

	var wg sync.WaitGroup
	var mu sync.Mutex
	success, noStock := 0, 0
	start := make(chan struct{})
	for _, e := range entries {
		wg.Add(1)
		go func(eid uint) {
			defer wg.Done()
			<-start
			_, err := svc.Confirm(eid)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case err.Error() == service.ErrNoStock.Error():
				noStock++
			default:
				t.Errorf("意外错误: %v", err)
			}
		}(e.ID)
	}
	close(start)
	wg.Wait()
	if success != 3 || noStock != 7 {
		t.Errorf("成功=%d 期望3，库存不足回滚=%d 期望7", success, noStock)
	}
	var prize models.Prize
	svc.DB().Where("campaign_id = ?", cid).First(&prize)
	if prize.Remaining != 0 {
		t.Errorf("库存出现负数或漏扣：%d", prize.Remaining)
	}
}

// 2) 正式名单唯一：串行与并发重复开奖都必须失败，DrawBatch 永远只有一份。
func TestOfficialDrawIsUnique(t *testing.T) {
	svc, cid, _ := setupWorld(t, 5, 15, 2, 1)
	b1, err := svc.Draw(cid)
	if err != nil {
		t.Fatalf("首次开奖失败: %v", err)
	}
	if _, err := svc.Draw(cid); err != service.ErrDuplicateDraw {
		t.Errorf("重复开奖应返回 ErrDuplicateDraw，实际=%v", err)
	}

	// 并发再来 8 次开奖请求，全部必须失败
	var wg sync.WaitGroup
	start := make(chan struct{})
	failOther := 0
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.Draw(cid); err != nil {
				mu.Lock()
				failOther++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if failOther != 8 {
		t.Errorf("并发重复开奖被拒次数=%d，期望 8", failOther)
	}
	var cnt int64
	svc.DB().Model(&models.DrawBatch{}).Where("campaign_id = ?", cid).Count(&cnt)
	if cnt != 1 {
		t.Errorf("正式开奖批次数=%d，必须恒为 1", cnt)
	}
	// 条目数与内容必须仍是首次开奖的结果
	entries := drawEntries(t, svc, cid)
	if len(entries) != 5 {
		t.Errorf("正式名单条目数=%d，期望 5", len(entries))
	}
	verify, err := svc.VerifyDraw(cid)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !verify["replay_matches"].(bool) || !verify["rule_hash_match"].(bool) {
		t.Errorf("名单重放/规则哈希校验失败: %+v", verify)
	}
	t.Logf("唯一批次 #%d 种子=%s，重放一致", b1.ID, b1.Seed[:16])
}

// 2b) 同一账号并发重复报名只能占一个名额。
func TestDuplicateRegistrationRejected(t *testing.T) {
	db := testDB(t)
	svc := service.New(db)
	simr := sim.New(svc)
	cid, err := svc.CreateCampaign("报名测试", 1, 2, 30,
		[]service.PrizeSpec{{Code: "A", Name: "奖", Stock: 2}}, time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := simr.SeedWorld(cid, 5, 1); err != nil {
		t.Fatal(err)
	}
	if err := svc.LockCampaign(cid); err != nil {
		t.Fatal(err)
	}
	if err := svc.OpenRegistration(cid); err != nil {
		t.Fatal(err)
	}
	var acc models.Account
	db.First(&acc)

	var wg sync.WaitGroup
	start := make(chan struct{})
	ok, dup := 0, 0
	var mu sync.Mutex
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.Register(cid, acc.ID)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				dup++
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok != 1 || dup != 9 {
		t.Errorf("报名成功=%d 期望1，被拒=%d 期望9", ok, dup)
	}
	var n int64
	db.Model(&models.Registration{}).Where("campaign_id = ? AND account_id = ?", cid, acc.ID).Count(&n)
	if n != 1 {
		t.Errorf("同账号报名记录数=%d，期望 1", n)
	}
}

// 3) 重复物流回执不能改变已确认的发货数量。
func TestDuplicateLogisticsReceiptIdempotent(t *testing.T) {
	svc, cid, _ := setupWorld(t, 3, 12, 2, 1)
	if _, err := svc.Draw(cid); err != nil {
		t.Fatal(err)
	}
	entries := drawEntries(t, svc, cid)
	e := entries[0]
	if _, err := svc.Confirm(e.ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := svc.SubmitAddress(e.ID, "张三", "13800000000", "测试市测试区 1 号"); err != nil {
		t.Fatalf("address: %v", err)
	}
	sh, err := svc.Ship(e.ID, "operator")
	if err != nil {
		t.Fatalf("ship: %v", err)
	}

	// 同一回执号并发投递 6 次
	receiptNo := "RCP-DUP-0001"
	var wg sync.WaitGroup
	start := make(chan struct{})
	dups, first := 0, 0
	var mu sync.Mutex
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dup, err := svc.ReceiveReceipt(receiptNo, sh.ShipmentNo, "delivered")
			if err != nil {
				t.Errorf("receipt error: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if dup {
				dups++
			} else {
				first++
			}
		}()
	}
	close(start)
	wg.Wait()
	if first != 1 || dups != 5 {
		t.Errorf("首次处理=%d 期望1，幂等重复=%d 期望5", first, dups)
	}
	// 再换一个新回执号重复妥投同一运单：状态不得二次推进
	dup2, err := svc.ReceiveReceipt("RCP-SECOND-0002", sh.ShipmentNo, "delivered")
	if err != nil {
		t.Fatal(err)
	}
	if dup2 {
		t.Errorf("不同回执号不应被视为号重复")
	}
	// 再重放一次旧回执号
	dup3, _ := svc.ReceiveReceipt(receiptNo, sh.ShipmentNo, "delivered")
	if !dup3 {
		t.Errorf("旧回执号重放必须判定为重复")
	}

	// 断言：发货单 1 张、已妥投、条目妥投、单件妥投、发货数量没有被放大
	var shipCnt int64
	svc.DB().Model(&models.Shipment{}).Where("entry_id = ?", e.ID).Count(&shipCnt)
	if shipCnt != 1 {
		t.Errorf("发货单数量=%d，期望 1（重复回执不得重复发货）", shipCnt)
	}
	var got models.Shipment
	svc.DB().First(&got, sh.ID)
	if got.Status != models.ShipmentDelivered {
		t.Errorf("运单状态=%s，期望 delivered", got.Status)
	}
	var ent models.DrawEntry
	svc.DB().First(&ent, e.ID)
	if ent.Status != models.EntryDelivered {
		t.Errorf("条目状态=%s，期望 delivered", ent.Status)
	}
	if ent.PrizeUnitID == nil {
		t.Fatal("条目未绑定奖品编号")
	}
	var unit models.PrizeUnit
	svc.DB().First(&unit, *ent.PrizeUnitID)
	if unit.Status != models.UnitDelivered {
		t.Errorf("单件状态=%s，期望 delivered", unit.Status)
	}
	var rcpt int64
	svc.DB().Model(&models.LogisticsReceipt{}).Where("receipt_no = ?", receiptNo).Count(&rcpt)
	if rcpt != 1 {
		t.Errorf("同一回执号落库 %d 条，期望 1", rcpt)
	}
}

// 4) 超期未填地址按锁定规则放弃并收回库存，但不产生新名单。
func TestForfeitFollowsLockedRulesNoRedraw(t *testing.T) {
	const N = 4
	svc, cid, _ := setupWorld(t, N, 12, 2, 1)
	if _, err := svc.Draw(cid); err != nil {
		t.Fatal(err)
	}
	entries := drawEntries(t, svc, cid)
	// 第 1 人确认+填址；第 2 人确认不填址；第 3、4 人完全不操作
	if _, err := svc.Confirm(entries[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.SubmitAddress(entries[0].ID, "李四", "139", "地址 9"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Confirm(entries[1].ID); err != nil {
		t.Fatal(err)
	}

	// 时限内巡检：什么都不处理
	if n := svc.ForfeitCampaign(cid, time.Now()); n != 0 {
		t.Errorf("时限内放弃数=%d，期望 0", n)
	}
	// 超过 30 分钟锁定时限后巡检：放弃 3 人
	n := svc.ForfeitCampaign(cid, time.Now().Add(40*time.Minute))
	if n != 3 {
		t.Errorf("超期放弃数=%d，期望 3", n)
	}
	statuses := map[string]int{}
	for _, e := range drawEntries(t, svc, cid) {
		statuses[e.Status]++
	}
	if statuses[models.EntryAddressed] != 1 || statuses[models.EntryForfeited] != 3 {
		t.Errorf("状态分布异常: %+v", statuses)
	}
	// 收回的奖品永久退出可发库存：1 件已填址待发（占用中）+ 3 件退出 = 可发余量 0
	var p models.Prize
	svc.DB().Where("campaign_id = ?", cid).First(&p)
	if p.Remaining != 0 {
		t.Errorf("退出后可发库存=%d，期望 0（1 件占用中 + 3 件永久收回）", p.Remaining)
	}
	var batches int64
	svc.DB().Model(&models.DrawBatch{}).Where("campaign_id = ?", cid).Count(&batches)
	if batches != 1 {
		t.Errorf("超期处理后批次数=%d，仍必须为 1（不补抽）", batches)
	}
	if _, err := svc.Draw(cid); err != service.ErrDuplicateDraw {
		t.Errorf("超期后重新开奖应被拒绝，实际=%v", err)
	}
}

// 5) 开奖后订单退货进入复核：名单保留、按阶段处置、绝不重抽。
func TestReturnOrderAfterDrawReview(t *testing.T) {
	const N = 4
	svc, cid, _ := setupWorld(t, N, 12, 2, 1)
	if _, err := svc.Draw(cid); err != nil {
		t.Fatal(err)
	}
	entries := drawEntries(t, svc, cid)

	// 待确认者退货：直接作废，不占库存
	var o1 models.Order
	svc.DB().Where("account_id = ? AND status = ?", entries[0].AccountID, "active").First(&o1)
	rc, err := svc.ReturnOrder(o1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Resolution != "void_pending" {
		t.Errorf("待确认退货处置=%s，期望 void_pending", rc.Resolution)
	}

	// 已确认待填址者退货：收回占用的奖品
	if _, err := svc.Confirm(entries[1].ID); err != nil {
		t.Fatal(err)
	}
	var o2 models.Order
	svc.DB().Where("account_id = ? AND status = ?", entries[1].AccountID, "active").First(&o2)
	rc2, err := svc.ReturnOrder(o2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rc2.Resolution != "forfeit_recovered" {
		t.Errorf("已确认退货处置=%s，期望 forfeit_recovered", rc2.Resolution)
	}

	after := drawEntries(t, svc, cid)
	if len(after) != N {
		t.Errorf("复核后名单条目数=%d，期望仍为 %d（名单不得被重写）", len(after), N)
	}
	var batches int64
	svc.DB().Model(&models.DrawBatch{}).Where("campaign_id = ?", cid).Count(&batches)
	if batches != 1 {
		t.Errorf("复核后批次数=%d，必须为 1", batches)
	}
	// 库存口径：4 件期初；entries0 待确认即作废退出 1 件（4→3）；
	// entries1 确认占用 1 件（3→2）后退货，占用件转退出、余量不回补；最终可发 2 件。
	var p models.Prize
	svc.DB().Where("campaign_id = ?", cid).First(&p)
	if p.Remaining != 2 {
		t.Errorf("复核后可发库存=%d，期望 2", p.Remaining)
	}
	consistency, err := svc.InventoryConsistent(cid)
	if err != nil {
		t.Fatal(err)
	}
	if !consistency["consistent"].(bool) {
		t.Errorf("库存对账不一致: %+v", consistency)
	}
}

// 6) 端到端小流程：发货与妥投后的对账必须全部为真。
func TestEndToEndInventoryConsistency(t *testing.T) {
	const N = 5
	svc, cid, _ := setupWorld(t, N, 15, 2, 1)
	if _, err := svc.Draw(cid); err != nil {
		t.Fatal(err)
	}
	entries := drawEntries(t, svc, cid)
	for i, e := range entries {
		if _, err := svc.Confirm(e.ID); err != nil {
			t.Fatalf("confirm %d: %v", i, err)
		}
		if err := svc.SubmitAddress(e.ID, fmt.Sprintf("用户%d", i), "139", "地址"); err != nil {
			t.Fatalf("address %d: %v", i, err)
		}
	}
	shipped := 0
	for i, e := range entries {
		if i%2 == 0 {
			sh, err := svc.Ship(e.ID, "operator")
			if err != nil {
				t.Fatalf("ship: %v", err)
			}
			if _, err := svc.ReceiveReceipt(fmt.Sprintf("RCP-E2E-%d", i), sh.ShipmentNo, "delivered"); err != nil {
				t.Fatal(err)
			}
			shipped++
		}
	}
	r, err := svc.InventoryConsistent(cid)
	if err != nil {
		t.Fatal(err)
	}
	if !r["consistent"].(bool) {
		t.Fatalf("库存对账失败: %+v", r)
	}
	prizes := r["prizes"].([]map[string]any)
	if prizes[0]["remaining"].(int) != 0 {
		t.Errorf("全部确认后剩余应为 0，实际 %v", prizes[0]["remaining"])
	}
	if prizes[0]["units_delivered"].(int64) != int64(shipped) {
		t.Errorf("妥投单件=%d 期望 %d", prizes[0]["units_delivered"], shipped)
	}
	// 排序确定性：同种子重放必须逐名次一致
	v, _ := svc.VerifyDraw(cid)
	if !v["replay_matches"].(bool) {
		t.Errorf("开奖重放不一致: %+v", v)
	}
}
