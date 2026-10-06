package service

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"livelottery/internal/models"
	"livelottery/internal/store"
)

// newTestSvc 每个用例使用独立的临时文件 SQLite（WAL，支持并发连接）
func newTestSvc(t *testing.T) *Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open(path, false)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return New(db)
}

// setupDrawnActivity 建活动→奖品→锁定→N人报名→结束→开奖
func setupDrawnActivity(t *testing.T, s *Service, winnerCount, stock, viewers int) (int64, int64, []string) {
	t.Helper()
	act, err := s.CreateActivity(CreateActivityInput{
		Name: "t", DrawAtISO: time.Now().Add(time.Hour).Format(time.RFC3339),
		MinAccountAgeDays: 0, NeedBoundPhone: false,
		WinnerCount: winnerCount, AddressHours: 24,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	prize, err := s.AddPrize(act.ID, "P", stock)
	if err != nil {
		t.Fatalf("prize: %v", err)
	}
	if _, err := s.LockActivity(act.ID); err != nil {
		t.Fatalf("lock: %v", err)
	}
	accs := make([]string, 0, viewers)
	for i := 0; i < viewers; i++ {
		a := fmt.Sprintf("u%03d", i)
		accs = append(accs, a)
		if _, err := s.Register(act.ID, ViewerProfile{AccountID: a, OrderNo: "O" + a}); err != nil {
			t.Fatalf("register %s: %v", a, err)
		}
	}
	if _, err := s.EndLive(act.ID); err != nil {
		t.Fatalf("end: %v", err)
	}
	return act.ID, prize.ID, accs
}

func addrFor(acc string) AddressInput {
	return AddressInput{Receiver: acc, Phone: "13800000000", Address: "测试地址-" + acc}
}

// TestConcurrentConfirmNoOversell 核心：并发确认（含重复提交）绝不超扣库存。
func TestConcurrentConfirmNoOversell(t *testing.T) {
	s := newTestSvc(t)
	const stock, winners = 5, 5
	actID, prizeID, _ := setupDrawnActivity(t, s, winners, stock, 12)
	res, err := s.Draw(actID)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	if len(res.Winners) != winners {
		t.Fatalf("中奖人数=%d 期望%d", len(res.Winners), winners)
	}
	winAccounts := make([]string, 0, winners)
	for _, w := range res.Winners {
		winAccounts = append(winAccounts, w.AccountID)
	}

	// 5 个中奖者，每人并发提交 10 次（重复点击/重试），共 50 个并发事务
	var wg sync.WaitGroup
	var okCount, dupOrNoStock int64
	for _, acc := range winAccounts {
		for k := 0; k < 10; k++ {
			wg.Add(1)
			go func(a string) {
				defer wg.Done()
				_, err := s.ConfirmWin(actID, a, addrFor(a))
				switch {
				case err == nil:
					atomic.AddInt64(&okCount, 1)
				default:
					atomic.AddInt64(&dupOrNoStock, 1)
				}
			}(acc)
		}
	}
	wg.Wait()

	// 断言 1：恰好 stock 件被占用，可发为 0
	avail, err := availableCount(s.db, prizeID)
	if err != nil {
		t.Fatal(err)
	}
	if avail != 0 {
		t.Fatalf("超扣/漏扣：剩余可用=%d，期望0", avail)
	}
	var claimed int64
	s.db.Model(&models.PrizeItem{}).Where("prize_id=? AND status=?", prizeID, models.ItemClaimed).Count(&claimed)
	if claimed != stock {
		t.Fatalf("占用数=%d 期望%d（超扣或空确认）", claimed, stock)
	}

	// 断言 2：恰好 winners 条 confirmed，且每条都绑定了不同的奖品编号
	var confirmed []models.WinnerRecord
	s.db.Where("activity_id=? AND status=?", actID, models.WinConfirmed).Find(&confirmed)
	if len(confirmed) != winners {
		t.Fatalf("confirmed 记录=%d 期望%d", len(confirmed), winners)
	}
	seenItem := map[int64]bool{}
	for _, w := range confirmed {
		if w.PrizeItemID == nil {
			t.Fatalf("中奖者 %s 已确认但无奖品编号（空确认）", w.AccountID)
		}
		if seenItem[*w.PrizeItemID] {
			t.Fatalf("奖品编号 %d 被分配给两个中奖者（超扣）", *w.PrizeItemID)
		}
		seenItem[*w.PrizeItemID] = true
	}

	// 断言 3：扣减流水恰好 stock 条 -1
	var debit int64
	s.db.Model(&models.StockLedger{}).
		Where("prize_id=? AND change=-1 AND reason='claim.confirm'", prizeID).Count(&debit)
	if debit != stock {
		t.Fatalf("扣减流水=%d 期望%d", debit, stock)
	}
	t.Logf("并发确认完成：成功返回=%d 其余幂等/无库存=%d，占用=%d 剩余=%d",
		okCount, dupOrNoStock, claimed, avail)
}

// TestConfirmScarceStockNoPhantomWin 实物不足时，并发确认不会出现“确认却无货”。
func TestConfirmScarceStockNoPhantomWin(t *testing.T) {
	s := newTestSvc(t)
	actID, prizeID, _ := setupDrawnActivity(t, s, 5, 5, 10)
	res, _ := s.Draw(actID)

	// 白盒：锁定后 3 件实物损毁（置为非 available），只剩 2 件可发
	var goneIDs []int64
	s.db.Model(&models.PrizeItem{}).Where("prize_id=?", prizeID).
		Order("id asc").Limit(3).Pluck("id", &goneIDs)
	if len(goneIDs) != 3 {
		t.Fatalf("预置损毁件数=%d", len(goneIDs))
	}
	s.db.Model(&models.PrizeItem{}).Where("id IN ?", goneIDs).Update("status", models.ItemReleased)

	var wg sync.WaitGroup
	var success int64
	var mu sync.Mutex
	phantom := []string{}
	for _, w := range res.Winners {
		wg.Add(1)
		go func(acc string) {
			defer wg.Done()
			out, err := s.ConfirmWin(actID, acc, addrFor(acc))
			if err == nil {
				atomic.AddInt64(&success, 1)
				if out != nil && out.PrizeItemID == nil {
					mu.Lock()
					phantom = append(phantom, acc)
					mu.Unlock()
				}
			}
		}(w.AccountID)
	}
	wg.Wait()

	if success != 2 {
		t.Fatalf("库存仅2，成功确认=%d，期望2", success)
	}
	if len(phantom) > 0 {
		t.Fatalf("出现确认中奖却无奖品编号: %v", phantom)
	}
	var confirmedNoItem int64
	s.db.Model(&models.WinnerRecord{}).
		Where("activity_id=? AND status=? AND prize_item_id IS NULL", actID, models.WinConfirmed).
		Count(&confirmedNoItem)
	if confirmedNoItem != 0 {
		t.Fatalf("存在 %d 条 confirmed 但无奖品编号", confirmedNoItem)
	}
}

// TestDrawIsUnique 并发开奖只有一份正式名单。
func TestDrawIsUnique(t *testing.T) {
	s := newTestSvc(t)
	actID, _, _ := setupDrawnActivity(t, s, 3, 3, 6)

	var wg sync.WaitGroup
	var success int64
	const n = 20
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Draw(actID); err == nil {
				atomic.AddInt64(&success, 1)
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("并发开奖成功次数=%d，期望恰好1", success)
	}
	var batchCount int64
	s.db.Model(&models.DrawBatch{}).Where("activity_id=?", actID).Count(&batchCount)
	if batchCount != 1 {
		t.Fatalf("正式名单批次=%d，期望1（重复开奖未被阻止）", batchCount)
	}
	// 串行再开一次也必须被拒
	if _, err := s.Draw(actID); err != ErrAlreadyDrawn {
		t.Fatalf("重复开奖 err=%v，期望 ErrAlreadyDrawn", err)
	}
}

// TestDuplicateReceiptNoDoubleShip 重复物流回执不改变已确认发货数量。
func TestDuplicateReceiptNoDoubleShip(t *testing.T) {
	s := newTestSvc(t)
	actID, _, _ := setupDrawnActivity(t, s, 1, 1, 3)
	res, _ := s.Draw(actID)
	w := res.Winners[0]
	cw, err := s.ConfirmWin(actID, w.AccountID, addrFor(w.AccountID))
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	ship, err := s.CreateShipment(actID, cw.ID, "TRACK-1")
	if err != nil {
		t.Fatalf("ship: %v", err)
	}

	// 同一回执号重复投递多次（并发）
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.HandleLogisticsReceipt(ReceiptEvent{
				ReceiptNo: "RCPT-1", TrackingNo: ship.TrackingNo, EventType: "delivered",
			})
		}()
	}
	wg.Wait()

	var receipts int64
	s.db.Model(&models.LogisticsReceipt{}).Where("receipt_no=?", "RCPT-1").Count(&receipts)
	if receipts != 1 {
		t.Fatalf("回执被登记 %d 次，期望1", receipts)
	}
	var shipCount int64
	s.db.Model(&models.Shipment{}).Where("winner_record_id=?", cw.ID).Count(&shipCount)
	if shipCount != 1 {
		t.Fatalf("发货单=%d，期望1（重复回执导致重复发货）", shipCount)
	}
	var got models.Shipment
	s.db.First(&got, ship.ID)
	if got.Status != models.ShipDelivered || got.DeliveredAt == nil {
		t.Fatalf("发货单状态=%q，期望 delivered", got.Status)
	}

	// 不同回执号的重复 delivered 也不能重复改变数量
	for i := 0; i < 3; i++ {
		_, err := s.HandleLogisticsReceipt(ReceiptEvent{
			ReceiptNo: fmt.Sprintf("RCPT-X%d", i), TrackingNo: ship.TrackingNo, EventType: "delivered",
		})
		if err != nil {
			t.Fatalf("后续回执: %v", err)
		}
	}
	s.db.Model(&models.Shipment{}).Where("winner_record_id=?", cw.ID).Count(&shipCount)
	if shipCount != 1 {
		t.Fatalf("重复送达后发货单=%d，期望1", shipCount)
	}

	// 重复创建发货单必须被拒（状态机或唯一约束），发货单仍为 1
	if _, err := s.CreateShipment(actID, cw.ID, "TRACK-2"); err == nil {
		t.Fatalf("重复发货应被拒绝")
	}
	s.db.Model(&models.Shipment{}).Where("winner_record_id=?", cw.ID).Count(&shipCount)
	if shipCount != 1 {
		t.Fatalf("拒绝重复发货后发货单=%d，期望1", shipCount)
	}
}

// TestDuplicateRegistration 同一账号并发报名只能占一个名额。
func TestDuplicateRegistration(t *testing.T) {
	s := newTestSvc(t)
	act, _ := s.CreateActivity(CreateActivityInput{
		Name: "t", DrawAtISO: time.Now().Add(time.Hour).Format(time.RFC3339),
		WinnerCount: 1, AddressHours: 24,
	})
	if _, err := s.AddPrize(act.ID, "P", 1); err != nil {
		t.Fatal(err)
	}
	s.LockActivity(act.ID)

	var wg sync.WaitGroup
	var success int64
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Register(act.ID, ViewerProfile{AccountID: "dup", OrderNo: "O"}); err == nil {
				atomic.AddInt64(&success, 1)
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("重复报名成功=%d，期望1", success)
	}
	var n int64
	s.db.Model(&models.Registration{}).
		Where("activity_id=? AND account_id=?", act.ID, "dup").Count(&n)
	if n != 1 {
		t.Fatalf("报名记录=%d，期望1", n)
	}
}

// TestRuleLockedImmutable 锁定后规则与库存不可改，且开奖使用锁定快照。
func TestRuleLockedImmutable(t *testing.T) {
	s := newTestSvc(t)
	act, _ := s.CreateActivity(CreateActivityInput{
		Name: "t", DrawAtISO: time.Now().Add(time.Hour).Format(time.RFC3339),
		WinnerCount: 2, AddressHours: 24,
	})
	s.AddPrize(act.ID, "P", 2)
	s.LockActivity(act.ID)

	before, _ := s.GetActivity(act.ID)
	snapBefore := before.RuleJSON
	if _, err := s.AddPrize(act.ID, "MORE", 5); err != ErrRuleLocked {
		t.Fatalf("锁定后加库存 err=%v，期望 ErrRuleLocked", err)
	}
	// 开奖后规则快照文本保持不变
	_, _ = s.Register(act.ID, ViewerProfile{AccountID: "a"})
	_, _ = s.Register(act.ID, ViewerProfile{AccountID: "b"})
	_, _ = s.Register(act.ID, ViewerProfile{AccountID: "c"})
	s.EndLive(act.ID)
	s.Draw(act.ID)
	after, _ := s.GetActivity(act.ID)
	if after.RuleJSON != snapBefore {
		t.Fatalf("规则快照在开奖后发生变化")
	}
}

// TestAddressTimeoutForfeit 超期未填地址按锁定原规则判弃奖、不补抽。
func TestAddressTimeoutForfeit(t *testing.T) {
	s := newTestSvc(t)
	actID, prizeID, _ := setupDrawnActivity(t, s, 2, 2, 4)
	res, _ := s.Draw(actID)

	// 注入未来时钟（超过 24h 时限）
	future := time.Now().UTC().Add(25 * time.Hour)
	s.WithClock(func() time.Time { return future })

	w0 := res.Winners[0]
	if _, err := s.ConfirmWin(actID, w0.AccountID, addrFor(w0.AccountID)); err != ErrAddressTimeout {
		t.Fatalf("超期确认 err=%v，期望 ErrAddressTimeout", err)
	}
	var wr models.WinnerRecord
	s.db.First(&wr, w0.ID)
	if wr.Status != models.WinForfeited {
		t.Fatalf("状态=%q，期望 forfeited", wr.Status)
	}
	// 弃奖没有占用库存；且没有补抽第三人
	avail, _ := availableCount(s.db, prizeID)
	if avail != 2 {
		t.Fatalf("弃奖后可用库存=%d，期望2", avail)
	}
	var nWinners int64
	s.db.Model(&models.WinnerRecord{}).Where("activity_id=?", actID).Count(&nWinners)
	if nWinners != 2 {
		t.Fatalf("弃奖后名单人数=%d，期望2（不应补抽）", nWinners)
	}

	// 批量扫描同样只判超期者
	n, err := s.ExpireOverdueWinners(actID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("扫描判弃奖人数=%d，期望1（已弃奖的不重复计数）", n)
	}
}

// TestReturnTriggersReviewAndNoRedraw 退货进入复核；判失效不产生新名单。
func TestReturnTriggersReviewAndNoRedraw(t *testing.T) {
	s := newTestSvc(t)
	actID, prizeID, accs := setupDrawnActivity(t, s, 2, 2, 4)
	res, _ := s.Draw(actID)
	loser := ""
	winSet := map[string]bool{}
	for _, w := range res.Winners {
		winSet[w.AccountID] = true
	}
	for _, a := range accs {
		if !winSet[a] {
			loser = a
			break
		}
	}

	// 未中奖者退货：报名进入复核
	if err := s.ReturnOrder(actID, loser, "ORD-"+loser); err != nil {
		t.Fatalf("return: %v", err)
	}
	var reg models.Registration
	s.db.Where("activity_id=? AND account_id=?", actID, loser).First(&reg)
	if reg.Status != models.RegInReview {
		t.Fatalf("退货后报名状态=%q，期望 in_review", reg.Status)
	}
	if err := s.ReviewDecision(actID, loser, false, "确认退货"); err != nil {
		t.Fatalf("review: %v", err)
	}

	// 中奖者在确认前退货→判失效→不占用库存、名单仍是原2人且不可重抽
	w0 := res.Winners[0]
	s.ReturnOrder(actID, w0.AccountID, "ORD-"+w0.AccountID)
	if err := s.ReviewDecision(actID, w0.AccountID, false, "确认退货"); err != nil {
		t.Fatalf("review winner: %v", err)
	}
	var wr models.WinnerRecord
	s.db.First(&wr, w0.ID)
	if wr.Status != models.WinDisqualified || wr.PrizeItemID != nil {
		t.Fatalf("中奖者状态=%q item=%v，期望 disqualified 且无占用", wr.Status, wr.PrizeItemID)
	}
	avail, _ := availableCount(s.db, prizeID)
	if avail != 2 {
		t.Fatalf("失效后可用库存=%d，期望2", avail)
	}
	if _, err := s.Draw(actID); err != ErrAlreadyDrawn {
		t.Fatalf("失效后重抽 err=%v，期望 ErrAlreadyDrawn（名单唯一不补抽）", err)
	}
}

// TestResultHashTamperEvident 名单被篡改时哈希校验失败。
func TestResultHashTamperEvident(t *testing.T) {
	s := newTestSvc(t)
	actID, _, _ := setupDrawnActivity(t, s, 3, 3, 6)
	s.Draw(actID)
	ok1, _, err := s.VerifyResultHash(actID)
	if err != nil || !ok1 {
		t.Fatalf("初始哈希校验应通过 ok=%v err=%v", ok1, err)
	}
	// 篡改一条名单账号
	var firstID int64
	s.db.Model(&models.WinnerRecord{}).Where("activity_id=?", actID).
		Order("rank asc").Limit(1).Pluck("id", &firstID)
	s.db.Model(&models.WinnerRecord{}).Where("id=?", firstID).Update("account_id", "hacker")
	ok2, _, _ := s.VerifyResultHash(actID)
	if ok2 {
		t.Fatalf("篡改后哈希校验不应通过")
	}
}
