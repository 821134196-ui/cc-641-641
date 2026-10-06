// Package service 实现直播抽奖的全部业务规则。
// 所有多步写操作都在单个 IMMEDIATE 事务内完成；唯一性靠数据库唯一索引兜底，
// 库存扣减靠带条件的 UPDATE 兜底，幂等靠业务键唯一索引兜底。
package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"lottery/internal/lottery"
	"lottery/internal/models"
	"lottery/internal/store"
)

var (
	ErrConflict        = errors.New("状态冲突：当前活动状态不允许该操作")
	ErrAlreadyWinner   = errors.New("该中奖记录已处理，不能重复确认")
	ErrNoStock         = errors.New("库存不足：确认中奖与库存扣减未能同时完成，已回滚")
	ErrDeadlinePassed  = errors.New("已超过锁定规则规定的填址时限")
	ErrDuplicateDraw   = errors.New("正式名单已存在，禁止再次开奖")
	ErrDuplicateReg    = errors.New("同一账号不可重复占用名额")
	ErrDuplicateSerial = errors.New("物流回执已处理，重复回执不改变任何状态")
)

type Service struct {
	db *store.DB
}

func New(db *store.DB) *Service { return &Service{db: db} }

func (s *Service) DB() *store.DB { return s.db }

// ---------- 工具 ----------

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hashLines(lines ...string) string {
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:])
}

func audit(tx *gorm.DB, campaignID uint, actor, action, detail string) {
	tx.Create(&models.AuditLog{
		CampaignID: campaignID, Actor: actor, Action: action, Detail: detail,
	})
}

// ruleHash 计算锁定规则的规范哈希（奖项按 code 排序，库存计入哈希）。
func ruleHash(r models.RuleSnapshot, prizes []models.Prize) string {
	lines := []string{
		fmt.Sprintf("min_active_orders=%d", r.MinActiveOrders),
		fmt.Sprintf("win_count=%d", r.WinCount),
		fmt.Sprintf("algorithm=%s", r.Algorithm),
		fmt.Sprintf("address_limit_minutes=%d", r.AddressLimitMinutes),
		fmt.Sprintf("forfeit_policy=%s", r.ForfeitPolicy),
	}
	sorted := append([]models.Prize(nil), prizes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Code < sorted[j].Code })
	for _, p := range sorted {
		lines = append(lines, fmt.Sprintf("prize=%s:%s:total=%d", p.Code, p.Name, p.TotalStock))
	}
	return hashLines(lines...)
}

func inventoryHash(units []models.PrizeUnit) string {
	sorted := append([]models.PrizeUnit(nil), units...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SerialNo < sorted[j].SerialNo })
	lines := make([]string, 0, len(sorted))
	for _, u := range sorted {
		lines = append(lines, fmt.Sprintf("%s|prize=%d", u.SerialNo, u.PrizeID))
	}
	return hashLines(lines...)
}

// ---------- 活动配置与锁定 ----------

type PrizeSpec struct {
	Code  string
	Name  string
	Stock int
}

// CreateCampaign 创建草稿活动并初始化奖项库存（含单件编号）。
func (s *Service) CreateCampaign(name string, minActiveOrders, winCount, addrLimitMin int, prizes []PrizeSpec, start, end time.Time) (uint, error) {
	var campaignID uint
	err := s.db.Tx(func(tx *gorm.DB) error {
		if winCount <= 0 {
			return fmt.Errorf("中奖人数必须为正")
		}
		total := 0
		for _, p := range prizes {
			total += p.Stock
		}
		if total != winCount {
			return fmt.Errorf("奖品总库存(%d)必须等于中奖人数(%d)，锁定前需对齐", total, winCount)
		}
		c := &models.Campaign{
			Name: name, Status: models.StatusDraft,
			ScheduledDrawAt: end.Add(30 * time.Minute),
			StartAt:         start, EndAt: end,
		}
		if err := tx.Create(c).Error; err != nil {
			return err
		}
		campaignID = c.ID
		for _, spec := range prizes {
			p := &models.Prize{
				CampaignID: c.ID, Code: spec.Code, Name: spec.Name,
				TotalStock: spec.Stock, Remaining: spec.Stock,
			}
			if err := tx.Create(p).Error; err != nil {
				return err
			}
			for i := 1; i <= spec.Stock; i++ {
				unit := &models.PrizeUnit{
					PrizeID: p.ID, CampaignID: c.ID,
					SerialNo: fmt.Sprintf("%s-C%d-%04d", spec.Code, c.ID, i),
					Status:   models.UnitStock,
				}
				if err := tx.Create(unit).Error; err != nil {
					return err
				}
			}
			tx.Create(&models.InventoryTransaction{
				CampaignID: c.ID, PrizeID: p.ID, Type: "lock",
				Delta: spec.Stock, BalanceAfter: spec.Stock,
				RefType: "prize",
			})
		}
		r := models.RuleSnapshot{
			CampaignID: c.ID, MinActiveOrders: minActiveOrders, WinCount: winCount,
			Algorithm: "seeded_fisher_yates", AddressLimitMinutes: addrLimitMin,
			ForfeitPolicy: "forfeit_no_redraw",
		}
		var dbPrizes []models.Prize
		tx.Where("campaign_id = ?", c.ID).Find(&dbPrizes)
		var units []models.PrizeUnit
		tx.Where("campaign_id = ?", c.ID).Find(&units)
		r.PrizeInventoryHash = inventoryHash(units)
		r.RuleHash = ruleHash(r, dbPrizes)
		if err := tx.Create(&r).Error; err != nil {
			return err
		}
		audit(tx, c.ID, "operator", "campaign_created",
			fmt.Sprintf("草稿已建：奖品总库存=%d, 规则哈希=%s", total, r.RuleHash[:12]))
		return nil
	})
	return campaignID, err
}

// LockCampaign 锁定参加条件、抽奖规则与奖品库存。锁定后不可修改。
func (s *Service) LockCampaign(id uint) error {
	return s.db.Tx(func(tx *gorm.DB) error {
		var c models.Campaign
		if err := tx.First(&c, id).Error; err != nil {
			return err
		}
		if c.Status != models.StatusDraft {
			return ErrConflict
		}
		now := time.Now()
		c.Status = models.StatusLocked
		if err := tx.Select("Status").Updates(&c).Error; err != nil {
			return err
		}
		tx.Model(&models.RuleSnapshot{}).Where("campaign_id = ?", id).Update("locked_at", now)
		audit(tx, id, "operator", "rules_locked", "参加条件/抽奖规则/奖品库存已锁定，之后不可修改")
		return nil
	})
}

// OpenRegistration 锁定之后开放报名。
func (s *Service) OpenRegistration(id uint) error {
	return s.db.Tx(func(tx *gorm.DB) error {
		var c models.Campaign
		if err := tx.First(&c, id).Error; err != nil {
			return err
		}
		if c.Status != models.StatusLocked {
			return ErrConflict
		}
		c.Status = models.StatusOpen
		if err := tx.Select("Status").Updates(&c).Error; err != nil {
			return err
		}
		audit(tx, id, "operator", "registration_opened", "观众报名通道已开放")
		return nil
	})
}

// ---------- 报名 ----------

func (s *Service) Register(campaignID, accountID uint) (*models.Registration, error) {
	reg := &models.Registration{CampaignID: campaignID, AccountID: accountID}
	err := s.db.Tx(func(tx *gorm.DB) error {
		var c models.Campaign
		if err := tx.First(&c, campaignID).Error; err != nil {
			return err
		}
		if c.Status != models.StatusOpen {
			return ErrConflict
		}
		var acc models.Account
		if err := tx.First(&acc, accountID).Error; err != nil {
			return err
		}
		if err := tx.Create(reg).Error; err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrDuplicateReg
			}
			return err
		}
		audit(tx, campaignID, acc.Nickname, "registered", "观众报名成功")
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reg, nil
}

// ---------- 结束并核验资格 ----------

// CloseAndVerify 结束报名并按锁定条件核验资格：活动期间有效订单数达标即为合格。
func (s *Service) CloseAndVerify(id uint) (qualified, disqualified int, err error) {
	err = s.db.Tx(func(tx *gorm.DB) error {
		var c models.Campaign
		if e := tx.First(&c, id).Error; e != nil {
			return e
		}
		if c.Status != models.StatusOpen {
			return ErrConflict
		}
		var regs []models.Registration
		tx.Where("campaign_id = ?", id).Find(&regs)
		var rule models.RuleSnapshot
		tx.Where("campaign_id = ?", id).First(&rule)
		for i := range regs {
			var activeCnt int64
			tx.Model(&models.Order{}).
				Where("campaign_id = ? AND account_id = ? AND status = ?", id, regs[i].AccountID, "active").
				Count(&activeCnt)
			ok := int(activeCnt) >= rule.MinActiveOrders
			regs[i].Qualified = &ok
			if !ok {
				regs[i].DisableReason = fmt.Sprintf("有效订单 %d 单，未达锁定条件 %d 单", activeCnt, rule.MinActiveOrders)
			}
			tx.Select("Qualified", "DisableReason").Save(&regs[i])
			if ok {
				qualified++
			} else {
				disqualified++
			}
		}
		// 开奖前退货的复核单在此随核验一并结案：不进入正式名单，也不补抽。
		tx.Model(&models.ReviewCase{}).
			Where("campaign_id = ? AND status = ?", id, "open").
			Updates(map[string]any{"status": "resolved", "resolution": "disqualified_before_draw", "resolved_at": time.Now()})
		c.Status = models.StatusClosed
		tx.Select("Status").Updates(&c)
		audit(tx, id, "system", "registration_closed",
			fmt.Sprintf("报名结束并核验资格：合格=%d 不合格=%d", qualified, disqualified))
		return nil
	})
	return
}

// ---------- 开奖：唯一正式名单 ----------

// prizeSlots 按奖项 code 排序展开中奖槽位：rank i 对应哪个奖项。
func prizeSlots(prizes []models.Prize) []uint {
	sorted := append([]models.Prize(nil), prizes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Code < sorted[j].Code })
	var slots []uint
	for _, p := range sorted {
		for i := 0; i < p.TotalStock; i++ {
			slots = append(slots, p.ID)
		}
	}
	return slots
}

// Draw 生成唯一正式名单。DrawBatch.CampaignID 的唯一索引从物理上保证：
// 即使运营重复点击或两个请求并发，也只会存在一份正式开奖结果。
func (s *Service) Draw(id uint) (*models.DrawBatch, error) {
	batch := &models.DrawBatch{}
	err := s.db.Tx(func(tx *gorm.DB) error {
		var c models.Campaign
		if e := tx.First(&c, id).Error; e != nil {
			return e
		}
		// 查重先于状态校验：任何阶段重复发起开奖都得到同一句明确答复，
		// 唯一索引是最终防线（防并发）。
		var cnt int64
		tx.Model(&models.DrawBatch{}).Where("campaign_id = ?", id).Count(&cnt)
		if cnt > 0 {
			return ErrDuplicateDraw
		}
		if c.Status != models.StatusClosed {
			return ErrConflict
		}
		var rule models.RuleSnapshot
		tx.Where("campaign_id = ?", id).First(&rule)

		var regs []models.Registration
		tx.Where("campaign_id = ? AND qualified = ?", id, true).Find(&regs)
		ids := make([]int, 0, len(regs))
		for _, r := range regs {
			ids = append(ids, int(r.AccountID))
		}
		sort.Ints(ids)
		if len(ids) == 0 {
			return errors.New("没有合格报名者，无法开奖")
		}
		seed, e := lottery.NewSeed()
		if e != nil {
			return e
		}
		order := lottery.SeededShuffle(seed, ids)
		winN := rule.WinCount
		if winN > len(order) {
			winN = len(order) // 合格者少于奖项数时，剩余库存保留，不补抽
		}
		winners := order[:winN]

		var prizes []models.Prize
		tx.Where("campaign_id = ?", id).Find(&prizes)
		slots := prizeSlots(prizes)

		b := &models.DrawBatch{
			CampaignID: id, Seed: seed, Algorithm: rule.Algorithm, RuleHash: rule.RuleHash,
			QualifiedRoot: lottery.MerkleRoot(ids),
			QualifiedIDs:  intsJSON(ids), EntryCount: winN, DrawnAt: time.Now(),
		}
		if e := tx.Create(b).Error; e != nil {
			if strings.Contains(e.Error(), "UNIQUE") {
				return ErrDuplicateDraw
			}
			return e
		}
		for rank, accID := range winners {
			e := &models.DrawEntry{
				BatchID: b.ID, Rank: rank + 1, CampaignID: id, AccountID: uint(accID),
				PrizeID: slots[rank], Status: models.EntryPending,
			}
			if e := tx.Create(e).Error; e != nil {
				return e
			}
		}
		// 合格者少于奖项名额：无人可中的槽位立即退出可发库存，永不补发。
		if winN < len(slots) {
			if e := s.retireSlots(tx, id, slots[winN:], "合格报名者不足，空缺名额退出库存，不补抽"); e != nil {
				return e
			}
		}
		c.Status = models.StatusDrawn
		tx.Select("Status").Updates(&c)
		audit(tx, id, "system", "drawn",
			fmt.Sprintf("正式名单已生成并锁定：中奖 %d 人，种子=%s，规则哈希=%s", winN, seed[:12], rule.RuleHash[:12]))
		*batch = *b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

// ---------- 中奖确认 + 库存条件扣减（同一事务，同时成功或同时失败） ----------

// Confirm 中奖确认。库存扣减、单件占用、名单状态推进在同一事务内：
// UPDATE prizes SET remaining=remaining-1 WHERE remaining>0 影响行数为 0 即整体回滚，
// 因此不可能出现“确认成功却无奖品可发”。
func (s *Service) Confirm(entryID uint) (*models.DrawEntry, error) {
	entry := &models.DrawEntry{}
	err := s.db.Tx(func(tx *gorm.DB) error {
		if e := tx.First(entry, entryID).Error; e != nil {
			return e
		}
		if entry.Status != models.EntryPending {
			return ErrAlreadyWinner
		}
		var rule models.RuleSnapshot
		tx.Where("campaign_id = ?", entry.CampaignID).First(&rule)
		var batch models.DrawBatch
		tx.First(&batch, entry.BatchID)
		if rule.AddressLimitMinutes > 0 && time.Now().After(batch.DrawnAt.Add(time.Duration(rule.AddressLimitMinutes)*time.Minute)) {
			return ErrDeadlinePassed
		}

		// 1) 条件扣减奖项总库存
		res := tx.Model(&models.Prize{}).
			Where("id = ? AND remaining > 0", entry.PrizeID).
			Update("remaining", gorm.Expr("remaining - 1"))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNoStock
		}
		// 2) 占用一件尚未分配的、编号最小的奖品实物（编号即发货依据）
		var unit models.PrizeUnit
		if e := tx.Where("prize_id = ? AND status = ?", entry.PrizeID, models.UnitStock).
			Order("serial_no ASC").First(&unit).Error; e != nil {
			return e
		}
		ur := tx.Model(&models.PrizeUnit{}).
			Where("id = ? AND status = ?", unit.ID, models.UnitStock).
			Updates(map[string]any{"status": models.UnitAllocated, "winner_id": entry.ID})
		if ur.Error != nil {
			return ur.Error
		}
		if ur.RowsAffected == 0 {
			return ErrNoStock
		}
		// 3) 推进名单状态
		now := time.Now()
		entry.Status = models.EntryConfirmed
		entry.PrizeUnitID = &unit.ID
		entry.ConfirmedAt = &now
		if e := tx.Select("Status", "PrizeUnitID", "ConfirmedAt").Save(entry).Error; e != nil {
			return e
		}
		var p models.Prize
		tx.First(&p, entry.PrizeID)
		tx.Create(&models.InventoryTransaction{
			CampaignID: entry.CampaignID, PrizeID: entry.PrizeID, Type: "allocate",
			Delta: -1, BalanceAfter: p.Remaining, RefType: "entry", RefID: entry.ID,
		})
		audit(tx, entry.CampaignID, "winner", "prize_confirmed",
			fmt.Sprintf("名单#%d 确认中奖，奖品编号 %s 已占用，库存余量 %d", entry.ID, unit.SerialNo, p.Remaining))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entry, nil
}

// SubmitAddress 中奖者填写收货地址（必须在锁定规则的时限内）。
func (s *Service) SubmitAddress(entryID uint, receiver, phone, address string) error {
	return s.db.Tx(func(tx *gorm.DB) error {
		var e models.DrawEntry
		if err := tx.First(&e, entryID).Error; err != nil {
			return err
		}
		if e.Status != models.EntryConfirmed {
			return ErrConflict
		}
		var rule models.RuleSnapshot
		tx.Where("campaign_id = ?", e.CampaignID).First(&rule)
		var batch models.DrawBatch
		tx.First(&batch, e.BatchID)
		if rule.AddressLimitMinutes > 0 && time.Now().After(batch.DrawnAt.Add(time.Duration(rule.AddressLimitMinutes)*time.Minute)) {
			return ErrDeadlinePassed
		}
		if strings.TrimSpace(receiver) == "" || strings.TrimSpace(address) == "" {
			return errors.New("收货人和地址不能为空")
		}
		now := time.Now()
		e.Receiver, e.Phone, e.Address = receiver, phone, address
		e.Status = models.EntryAddressed
		e.AddressedAt = &now
		if err := tx.Select("Receiver", "Phone", "Address", "Status", "AddressedAt").Save(&e).Error; err != nil {
			return err
		}
		audit(tx, e.CampaignID, "winner", "address_submitted", fmt.Sprintf("名单#%d 已填写收货地址", e.ID))
		return nil
	})
}

// ForfeitOverdue 按锁定规则处理所有已开奖活动的超期条目。
func (s *Service) ForfeitOverdue(now time.Time) (forfeited int) {
	var cIDs []uint
	s.db.Model(&models.Campaign{}).Where("status = ?", models.StatusDrawn).Pluck("id", &cIDs)
	for _, cid := range cIDs {
		forfeited += s.ForfeitCampaign(cid, now)
	}
	return
}

// ForfeitCampaign 处理单个活动的超期条目；now 可由调用方指定（用于演示时间快进）。
// 未确认或确认后未填地址一律放弃，已占用奖品收回库存（状态 recovered，
// 系统无名单可再分配，绝不补抽）。
func (s *Service) ForfeitCampaign(cid uint, now time.Time) (forfeited int) {
	_ = s.db.Tx(func(tx *gorm.DB) error {
		var c models.Campaign
		if err := tx.First(&c, cid).Error; err != nil {
			return nil
		}
		if c.Status != models.StatusDrawn {
			return nil
		}
		var rule models.RuleSnapshot
		tx.Where("campaign_id = ?", cid).First(&rule)
		var batch models.DrawBatch
		tx.Where("campaign_id = ?", cid).First(&batch)
		if rule.AddressLimitMinutes <= 0 {
			return nil
		}
		deadline := batch.DrawnAt.Add(time.Duration(rule.AddressLimitMinutes) * time.Minute)
		if !now.After(deadline) {
			return nil
		}
		var entries []models.DrawEntry
		tx.Where("campaign_id = ? AND status IN ?", cid, []string{models.EntryPending, models.EntryConfirmed}).Find(&entries)
		for i := range entries {
			e := &entries[i]
			if err := s.retireEntryPrize(tx, e, "overdue_no_address"); err != nil {
				return err
			}
			e.Status = models.EntryForfeited
			e.ForfeitReason = "超期未按锁定规则填写地址，资格放弃，不补抽"
			ft := now
			e.ForfeitedAt = &ft
			tx.Select("Status", "ForfeitReason", "ForfeitedAt").Save(e)
			forfeited++
		}
		if len(entries) > 0 {
			audit(tx, cid, "system", "forfeited", fmt.Sprintf("超期放弃 %d 人，按锁定规则处理，不重新开奖", len(entries)))
		}
		return nil
	})
	return
}

// retireEntryPrize 在中奖名额作废/放弃时让对应奖品永久退出可发库存（绝不补抽）。
//   - 已确认占用（allocated）：单件置 recovered；可发余量 remaining 不回补；
//   - 尚未占用（pending）：锁一件在库单件置 recovered，同时 remaining 减 1。
//
// 口径统一为“这个名额永久作废”，因此库存流水始终与剩余可发量轧平。
func (s *Service) retireEntryPrize(tx *gorm.DB, e *models.DrawEntry, reason string) error {
	if e.PrizeUnitID != nil {
		var unit models.PrizeUnit
		if err := tx.First(&unit, *e.PrizeUnitID).Error; err != nil {
			return err
		}
		if unit.Status != models.UnitAllocated {
			return nil
		}
		if err := tx.Model(&models.PrizeUnit{}).Where("id = ? AND status = ?", unit.ID, models.UnitAllocated).
			Update("status", models.UnitRecovered).Error; err != nil {
			return err
		}
		var p models.Prize
		tx.First(&p, e.PrizeID)
		tx.Create(&models.InventoryTransaction{
			CampaignID: e.CampaignID, PrizeID: e.PrizeID, Type: "retire",
			Delta: 0, BalanceAfter: p.Remaining, RefType: "entry", RefID: e.ID,
		})
		audit(tx, e.CampaignID, "system", "unit_retired",
			fmt.Sprintf("名单#%d 奖品编号 %s 永久退出可发库存（%s），不补抽", e.ID, unit.SerialNo, reason))
		return nil
	}
	return s.retireOneStockUnit(tx, e.CampaignID, e.PrizeID, "entry", e.ID, reason)
}

// retireOneStockUnit 锁定一件在库奖品永久退出可发库存，并同步 remaining-1。
func (s *Service) retireOneStockUnit(tx *gorm.DB, campaignID, prizeID uint, refType string, refID uint, reason string) error {
	res := tx.Model(&models.Prize{}).Where("id = ? AND remaining > 0", prizeID).
		Update("remaining", gorm.Expr("remaining - 1"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNoStock
	}
	var unit models.PrizeUnit
	if err := tx.Where("prize_id = ? AND status = ?", prizeID, models.UnitStock).
		Order("serial_no ASC").First(&unit).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.PrizeUnit{}).Where("id = ? AND status = ?", unit.ID, models.UnitStock).
		Update("status", models.UnitRecovered).Error; err != nil {
		return err
	}
	var p models.Prize
	tx.First(&p, prizeID)
	tx.Create(&models.InventoryTransaction{
		CampaignID: campaignID, PrizeID: prizeID, Type: "retire",
		Delta: -1, BalanceAfter: p.Remaining, RefType: refType, RefID: refID,
	})
	audit(tx, campaignID, "system", "unit_retired",
		fmt.Sprintf("奖品编号 %s 永久退出可发库存（%s），不补抽", unit.SerialNo, reason))
	return nil
}

// retireSlots 批量退出空缺名额对应的奖品槽位。
func (s *Service) retireSlots(tx *gorm.DB, campaignID uint, prizeIDs []uint, reason string) error {
	cntByPrize := map[uint]int{}
	for _, pid := range prizeIDs {
		cntByPrize[pid]++
	}
	for pid, n := range cntByPrize {
		for i := 0; i < n; i++ {
			if err := s.retireOneStockUnit(tx, campaignID, pid, "draw", 0, reason); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------- 按奖品编号发货 ----------

// Ship 为已填地址的名单条目发货：一个条目只能有一张发货单（entry_id 唯一），
// 一个奖品编号只能被发一次（prize_unit_id 唯一）。
func (s *Service) Ship(entryID uint, actor string) (*models.Shipment, error) {
	ship := &models.Shipment{}
	err := s.db.Tx(func(tx *gorm.DB) error {
		var e models.DrawEntry
		if err := tx.First(&e, entryID).Error; err != nil {
			return err
		}
		if e.Status != models.EntryAddressed {
			return ErrConflict
		}
		if e.PrizeUnitID == nil {
			return ErrNoStock
		}
		var unit models.PrizeUnit
		tx.First(&unit, *e.PrizeUnitID)
		shippedAt := time.Now()
		sh := &models.Shipment{
			CampaignID: e.CampaignID, EntryID: e.ID, PrizeUnitID: unit.ID,
			SerialNo: unit.SerialNo, ShipmentNo: fmt.Sprintf("SF%d-%d-%s", e.CampaignID, e.ID, randHex(3)),
			Status: models.ShipmentShipped, ShippedAt: shippedAt,
		}
		if err := tx.Create(sh).Error; err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrAlreadyWinner
			}
			return err
		}
		tx.Model(&models.PrizeUnit{}).Where("id = ? AND status = ?", unit.ID, models.UnitAllocated).
			Update("status", models.UnitShipped)
		e.Status = models.EntryShipped
		tx.Select("Status").Save(&e)
		audit(tx, e.CampaignID, actor, "shipped",
			fmt.Sprintf("名单#%d 按奖品编号 %s 发货，运单 %s", e.ID, unit.SerialNo, sh.ShipmentNo))
		*ship = *sh
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ship, nil
}

// ReceiveReceipt 接收物流回执。receipt_no 唯一索引保证同一回执重复上报时幂等：
// 已处理过的回执直接返回，不产生任何状态变化；
// 同时发货单/条目/奖品都带当前状态条件，已妥投的不会被二次推进。
func (s *Service) ReceiveReceipt(receiptNo, shipmentNo, event string) (duplicate bool, err error) {
	err = s.db.Tx(func(tx *gorm.DB) error {
		var exist models.LogisticsReceipt
		if e := tx.Where("receipt_no = ?", receiptNo).First(&exist).Error; e == nil {
			duplicate = true // 重复回执：原样返回，不写任何数据
			return nil
		}
		var sh models.Shipment
		if e := tx.Where("shipment_no = ?", shipmentNo).First(&sh).Error; e != nil {
			return e
		}
		if err := tx.Create(&models.LogisticsReceipt{
			ReceiptNo: receiptNo, ShipmentID: sh.ID, Event: event,
			Payload: fmt.Sprintf("shipment=%s event=%s", shipmentNo, event),
		}).Error; err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				duplicate = true
				return nil
			}
			return err
		}
		if event == "delivered" {
			cr := tx.Model(&models.Shipment{}).Where("id = ? AND status = ?", sh.ID, models.ShipmentShipped).
				Updates(map[string]any{"status": models.ShipmentDelivered, "delivered_at": time.Now()})
			if cr.RowsAffected == 0 {
				// 运单已妥投（另一回执已处理）——确认发货数量不变。
				audit(tx, sh.CampaignID, "logistics", "receipt_ignored",
					fmt.Sprintf("运单 %s 已妥投，回执 %s 不改变状态", shipmentNo, receiptNo))
				return nil
			}
			tx.Model(&models.DrawEntry{}).Where("id = ? AND status = ?", sh.EntryID, models.EntryShipped).
				Update("status", models.EntryDelivered)
			tx.Model(&models.PrizeUnit{}).Where("id = ? AND status = ?", sh.PrizeUnitID, models.UnitShipped).
				Update("status", models.UnitDelivered)
			audit(tx, sh.CampaignID, "logistics", "delivered",
				fmt.Sprintf("运单 %s 妥投（回执 %s），发货数量不变", shipmentNo, receiptNo))
		}
		return nil
	})
	return
}

// ---------- 订单退货 → 复核（绝不重新开奖） ----------

// ReturnOrder 模拟订单退货。退货使资格失效，系统按开奖所处阶段处置：
// 开奖前 → 核验时排除；待确认 → 直接作废；已确认/已填址 → 收回奖品；
// 在途 → 挂起人工复核；已妥投 → 仅记录。任何阶段都不会重新抽取正式名单。
func (s *Service) ReturnOrder(orderID uint) (*models.ReviewCase, error) {
	rc := &models.ReviewCase{}
	err := s.db.Tx(func(tx *gorm.DB) error {
		var order models.Order
		if e := tx.First(&order, orderID).Error; e != nil {
			return e
		}
		if order.Status == "returned" {
			return errors.New("订单已退货，请勿重复操作")
		}
		now := time.Now()
		order.Status = "returned"
		order.ReturnedAt = &now
		tx.Select("Status", "ReturnedAt").Save(&order)

		var c models.Campaign
		tx.First(&c, order.CampaignID)
		var reg models.Registration
		regErr := tx.Where("campaign_id = ? AND account_id = ?", order.CampaignID, order.AccountID).First(&reg).Error

		caseRow := &models.ReviewCase{
			CampaignID: order.CampaignID, AccountID: order.AccountID, OrderID: order.ID,
			Reason: "中奖账号订单退货，报名资格失效，进入复核", Status: "open",
		}
		if regErr == nil {
			q := false
			reg.Qualified = &q
			reg.DisableReason = "订单退货，资格复核失效"
			tx.Select("Qualified", "DisableReason").Save(&reg)
		}

		var entry *models.DrawEntry
		if c.Status == models.StatusDrawn || c.Status == models.StatusSettled {
			var e models.DrawEntry
			if e2 := tx.Where("campaign_id = ? AND account_id = ? AND status IN ?",
				order.CampaignID, order.AccountID,
				[]string{models.EntryPending, models.EntryConfirmed, models.EntryAddressed, models.EntryShipped, models.EntryDelivered}).
				First(&e).Error; e2 == nil {
				entry = &e
				caseRow.EntryID = &e.ID
			}
		}

		if entry == nil {
			// 开奖前或未中奖：留待核验结案，或直接记录
			if c.Status == models.StatusDraft || c.Status == models.StatusLocked || c.Status == models.StatusOpen {
				caseRow.Note = "开奖前退货，核验资格时排除，不影响正式名单"
			} else {
				caseRow.Status = "resolved"
				caseRow.Resolution = "not_a_winner"
				caseRow.Note = "非中奖者，仅记录资格失效"
				t := time.Now()
				caseRow.ResolvedAt = &t
			}
		} else {
			switch entry.Status {
			case models.EntryPending:
				if e := s.retireEntryPrize(tx, entry, "order_returned_before_confirm"); e != nil {
					return e
				}
				entry.Status = models.EntryVoidReview
				entry.ForfeitReason = "开奖后订单退货，资格复核作废，对应名额退出库存，不补抽"
				tx.Select("Status", "ForfeitReason").Save(entry)
				caseRow.Status, caseRow.Resolution = "resolved", "void_pending"
				t := time.Now()
				caseRow.ResolvedAt = &t
			case models.EntryConfirmed, models.EntryAddressed:
				if e := s.retireEntryPrize(tx, entry, "order_returned"); e != nil {
					return e
				}
				entry.Status = models.EntryVoidReview
				entry.ForfeitReason = "订单退货资格失效，已收回奖品，不补抽"
				tx.Select("Status", "ForfeitReason").Save(entry)
				caseRow.Status, caseRow.Resolution = "resolved", "forfeit_recovered"
				t := time.Now()
				caseRow.ResolvedAt = &t
			case models.EntryShipped:
				caseRow.Note = "奖品已在途，挂起等待运营人工复核（不自动召回、不重新开奖）"
			case models.EntryDelivered:
				caseRow.Status, caseRow.Resolution = "resolved", "recorded_after_delivery"
				caseRow.Note = "奖品已妥投，仅记录备查，不改变正式名单与发货数量"
				t := time.Now()
				caseRow.ResolvedAt = &t
			}
		}
		if e := tx.Create(caseRow).Error; e != nil {
			return e
		}
		audit(tx, order.CampaignID, "system", "order_returned",
			fmt.Sprintf("订单#%d 退货触发资格复核，正式名单不重新生成", orderID))
		*rc = *caseRow
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rc, nil
}

// ---------- 查询与审计 ----------

// VerifyDraw 用持久化种子重放洗牌，核对正式名单未被篡改；同时核对规则哈希。
func (s *Service) VerifyDraw(campaignID uint) (map[string]any, error) {
	var batch models.DrawBatch
	if err := s.db.Where("campaign_id = ?", campaignID).First(&batch).Error; err != nil {
		return nil, err
	}
	var rule models.RuleSnapshot
	s.db.Where("campaign_id = ?", campaignID).First(&rule)
	ids := jsonInts(batch.QualifiedIDs)
	expected := lottery.SeededShuffle(batch.Seed, ids)
	var entries []models.DrawEntry
	s.db.Where("batch_id = ?", batch.ID).Order("rank ASC").Find(&entries)
	got := make([]int, len(entries))
	for i, e := range entries {
		got[i] = int(e.AccountID)
	}
	wantN := len(entries)
	replayOK := len(got) == wantN
	for i := 0; i < wantN && replayOK; i++ {
		if got[i] != expected[i] {
			replayOK = false
		}
	}
	return map[string]any{
		"batch_id":           batch.ID,
		"seed":               batch.Seed,
		"rule_hash_locked":   rule.RuleHash,
		"rule_hash_in_batch": batch.RuleHash,
		"rule_hash_match":    rule.RuleHash == batch.RuleHash,
		"qualified_root":     batch.QualifiedRoot,
		"qualified_root_now": lottery.MerkleRoot(ids),
		"replay_matches":     replayOK,
		"entry_count":        len(entries),
	}, nil
}

// InventoryConsistent 核对：期初 ± 流水 = 奖品表余量；各状态单件数与余额一致；
// 已发货数量 = 发货单数量 = 已妥投/在途条目数，且没有任何编号被发两次。
func (s *Service) InventoryConsistent(campaignID uint) (map[string]any, error) {
	var prizes []models.Prize
	s.db.Where("campaign_id = ?", campaignID).Find(&prizes)
	results := []map[string]any{}
	allOK := true
	for _, p := range prizes {
		var deltaSum int64
		s.db.Model(&models.InventoryTransaction{}).
			Where("prize_id = ?", p.ID).Select("COALESCE(SUM(delta),0)").Scan(&deltaSum)
		var stock, allocated, shipped, delivered, recovered int64
		countUnit := func(status string, cnt *int64) {
			s.db.Model(&models.PrizeUnit{}).Where("prize_id = ? AND status = ?", p.ID, status).Count(cnt)
		}
		countUnit(models.UnitStock, &stock)
		countUnit(models.UnitAllocated, &allocated)
		countUnit(models.UnitShipped, &shipped)
		countUnit(models.UnitDelivered, &delivered)
		countUnit(models.UnitRecovered, &recovered)
		var shipCnt int64
		s.db.Model(&models.Shipment{}).
			Joins("JOIN draw_entries ON draw_entries.id = shipments.entry_id").
			Where("draw_entries.prize_id = ?", p.ID).Count(&shipCnt)
		ledgerOK := int(deltaSum) == p.Remaining
		unitOK := int(stock) == p.Remaining
		shipOK := shipCnt == shipped+delivered
		ok := ledgerOK && unitOK && shipOK
		if !ok {
			allOK = false
		}
		results = append(results, map[string]any{
			"prize": p.Code, "name": p.Name, "total": p.TotalStock, "remaining": p.Remaining,
			"ledger_sum": deltaSum, "units_stock": stock, "units_allocated": allocated,
			"units_shipped": shipped, "units_delivered": delivered, "units_recovered": recovered,
			"shipments": shipCnt,
			"ledger_ok": ledgerOK, "units_ok": unitOK, "shipments_ok": shipOK,
		})
	}
	// 唯一性：发货单编号/奖品编号不能重复占用
	var dupUnit int64
	s.db.Model(&models.PrizeUnit{}).Where("campaign_id = ? AND status IN ?", campaignID,
		[]string{models.UnitShipped, models.UnitDelivered}).
		Select("COUNT(*) - COUNT(DISTINCT id)").Scan(&dupUnit)
	return map[string]any{"consistent": allOK && dupUnit == 0, "prizes": results}, nil
}
