package service

import (
	"fmt"
	"time"

	"gorm.io/gorm"
	"livelottery/internal/models"
)

// AddressInput 中奖者填写的收货信息
type AddressInput struct {
	Receiver string
	Phone    string
	Address  string
}

// ConfirmWin 中奖确认：填写地址与占用奖品编号必须在同一个事务里同时成功或同时失败。
//
// 防超扣机制（三层）：
//  1. _txlock=immediate：并发事务在第一句即被串行化；
//  2. SELECT ... 取一条 available 编号并 UPDATE status=claimed，
//     条件带 status='available'，affected rows 必须为 1，否则回滚（无库存）；
//  3. prize_items.winner_record_id 唯一 + winner_records.prize_item_id 唯一，
//     一个编号不可能同时归属两人。
//
// 因此绝不可能出现“确认了中奖却无奖品可发”。
func (s *Service) ConfirmWin(activityID int64, accountID string, in AddressInput) (*models.WinnerRecord, error) {
	if in.Receiver == "" || in.Phone == "" || in.Address == "" {
		return nil, fmt.Errorf("收货人、电话、地址均必填")
	}
	var out *models.WinnerRecord
	timeout := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		act, err := getActivity(tx, activityID)
		if err != nil {
			return err
		}
		rule, err := act.Rule()
		if err != nil {
			return err
		}
		var win models.WinnerRecord
		if err := tx.Where("activity_id = ? AND account_id = ?", activityID, accountID).First(&win).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return ErrWinnerIneligible
			}
			return err
		}

		switch win.Status {
		case models.WinReviewing:
			return ErrInReview
		case models.WinConfirmed, models.WinShipped, models.WinDelivered:
			return nil // 已确认，幂等返回（不重复扣库存）
		case models.WinForfeited, models.WinDisqualified:
			return ErrWinnerNotPending
		case models.WinPending:
			// 继续处理
		default:
			return ErrWinnerNotPending
		}

		// 超期未填地址：按锁定的原规则处理（forfeit：弃奖、释放名额、不补抽）。
		// 注意：弃奖状态必须提交，因此事务回调返回 nil，事务外再返回业务错误码。
		deadline := win.CreatedAt.Add(hours(rule.AddressDeadlineHours))
		if s.now().After(deadline) {
			now := s.now()
			if err := tx.Model(&win).Updates(map[string]any{
				"status":       models.WinForfeited,
				"forfeited_at": now,
			}).Error; err != nil {
				return err
			}
			auditTx(tx, activityID, accountID, "win.forfeit",
				fmt.Sprintf("超过锁定时限%.1fh未填地址，弃奖不补抽", rule.AddressDeadlineHours))
			timeout = true
			return nil
		}

		// 原子占用一件 available 奖品编号
		var item models.PrizeItem
		if err := tx.Where("prize_id = ? AND status = ?", win.PrizeID, models.ItemAvailable).
			Order("id asc").First(&item).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return ErrNoStock
			}
			return err
		}
		res := tx.Model(&models.PrizeItem{}).
			Where("id = ? AND status = ?", item.ID, models.ItemAvailable).
			Updates(map[string]any{"status": models.ItemClaimed, "winner_record_id": win.ID})
		if res.Error != nil {
			if isUniqueViolation(res.Error) {
				return ErrNoStock
			}
			return res.Error
		}
		if res.RowsAffected != 1 {
			// 被并发事务抢先：本事务拿不到实物，回滚，绝不“空确认”
			return ErrNoStock
		}

		now := s.now()
		if err := tx.Model(&win).Updates(map[string]any{
			"status":   models.WinConfirmed,
			"receiver": in.Receiver, "phone": in.Phone, "address": in.Address,
			"confirmed_at": now, "prize_item_id": item.ID,
		}).Error; err != nil {
			return err
		}
		if err := debitLedger(tx, win.PrizeID, "claim.confirm", win.ID); err != nil {
			return err
		}
		auditTx(tx, activityID, accountID, "win.confirm",
			fmt.Sprintf("确认中奖并占用奖品编号 %s", item.SerialNo))

		win.Status = models.WinConfirmed
		win.PrizeItemID = &item.ID
		win.Receiver, win.Phone, win.Address = in.Receiver, in.Phone, in.Address
		out = &win
		return nil
	})
	if err != nil {
		return nil, err
	}
	if timeout {
		// 弃奖已随事务提交；向调用方返回业务错误码
		return nil, ErrAddressTimeout
	}
	if out == nil {
		// 命中幂等分支：回读最新记录
		var w models.WinnerRecord
		if e := s.db.Where("activity_id = ? AND account_id = ?", activityID, accountID).First(&w).Error; e == nil {
			return &w, nil
		}
	}
	return out, nil
}

// ExpireOverdueWinners 扫描超期未填地址的待确认中奖者，按锁定规则判弃奖。
func (s *Service) ExpireOverdueWinners(activityID int64) (int, error) {
	expired := 0
	err := s.db.Transaction(func(tx *gorm.DB) error {
		act, err := getActivity(tx, activityID)
		if err != nil {
			return err
		}
		rule, err := act.Rule()
		if err != nil {
			return err
		}
		var pendings []models.WinnerRecord
		if err := tx.Where("activity_id = ? AND status = ?", activityID, models.WinPending).
			Find(&pendings).Error; err != nil {
			return err
		}
		cutoff := s.now().Add(-hours(rule.AddressDeadlineHours))
		for i := range pendings {
			w := pendings[i]
			if w.CreatedAt.Before(cutoff) {
				if err := tx.Model(&w).Updates(map[string]any{
					"status": models.WinForfeited, "forfeited_at": s.now(),
				}).Error; err != nil {
					return err
				}
				auditTx(tx, activityID, w.AccountID, "win.forfeit",
					"系统按锁定规则判超期弃奖，不补抽")
				expired++
			}
		}
		return nil
	})
	return expired, err
}

func hours(f float64) time.Duration {
	return time.Duration(f * float64(time.Hour))
}
