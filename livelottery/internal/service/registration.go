package service

import (
	"fmt"

	"gorm.io/gorm"
	"livelottery/internal/models"
)

// ViewerProfile 模拟直播账号资料（本地模拟，无外部依赖）
type ViewerProfile struct {
	AccountID      string
	OrderNo        string // 关联订单
	AccountAgeDays int
	PhoneBound     bool
}

// Register 观众报名。
// 不变量：
//  1. 仅在 locked 阶段报名（规则先锁定）；
//  2. 按锁定快照核验参加条件；
//  3. (activity_id, account_id) 唯一索引保证同一账号不能重复占名额。
func (s *Service) Register(activityID int64, p ViewerProfile) (*models.Registration, error) {
	if p.AccountID == "" {
		return nil, fmt.Errorf("账号必填")
	}
	reg := &models.Registration{}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		act, err := getActivity(tx, activityID)
		if err != nil {
			return err
		}
		if act.Status != models.StatusLocked {
			return ErrRegistrationClosed
		}
		rule, err := act.Rule()
		if err != nil {
			return err
		}
		// 按锁定的参加条件核验
		if p.AccountAgeDays < rule.MinAccountAgeDays {
			return ErrNotEligible
		}
		if rule.NeedBoundPhone && !p.PhoneBound {
			return ErrNotEligible
		}
		reg.ActivityID = activityID
		reg.AccountID = p.AccountID
		reg.OrderNo = p.OrderNo
		reg.Status = models.RegRegistered
		reg.AccountAgeDays = p.AccountAgeDays
		reg.PhoneBound = p.PhoneBound
		if err := tx.Create(reg).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrAlreadyRegistered
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.audit(activityID, p.AccountID, "register", "报名占名额 订单:"+p.OrderNo)
	return reg, nil
}

// ReturnOrder 模拟“订单退货”事件：导致报名资格失效，进入复核而非直接删除。
// 若该账号已中奖，则中奖记录一并进入 reviewing；不影响正式名单本身。
func (s *Service) ReturnOrder(activityID int64, accountID, orderNo string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		act, err := getActivity(tx, activityID)
		if err != nil {
			return err
		}
		rule, err := act.Rule()
		if err != nil {
			return err
		}

		var reg models.Registration
		res := tx.Where("activity_id = ? AND account_id = ?", activityID, accountID).First(&reg)
		if res.Error == gorm.ErrRecordNotFound {
			return ErrWinnerIneligible
		} else if res.Error != nil {
			return res.Error
		}
		// 幂等：已在复核/已判失效则不重复处理
		if reg.Status == models.RegInReview || reg.Status == models.RegIneligible {
			return nil
		}
		note := fmt.Sprintf("订单 %s 退货，资格待复核", orderNo)
		updates := map[string]any{"status": models.RegInReview, "review_note": note}
		if err := tx.Model(&models.Registration{}).Where("id = ?", reg.ID).Updates(updates).Error; err != nil {
			return err
		}

		// 若已中奖，中奖记录进入复核（冻结后续发货）
		var win models.WinnerRecord
		if err := tx.Where("activity_id = ? AND account_id = ?", activityID, accountID).First(&win).Error; err == nil {
			if win.Status == models.WinPending || win.Status == models.WinConfirmed {
				if err := tx.Model(&models.WinnerRecord{}).Where("id = ?", win.ID).
					Update("status", models.WinReviewing).Error; err != nil {
					return err
				}
			}
		}

		auditTx(tx, activityID, accountID, "order.return",
			fmt.Sprintf("订单%s退货；规则退货失效=%v", orderNo, rule.ReturnInvalidatesEligibility))
		return nil
	})
}

// ReviewDecision 运营复核结论。confirm=true 恢复资格，false 判定失效。
// 无论结论如何，都不改变正式名单的组成；已取消的中奖名额不补抽。
func (s *Service) ReviewDecision(activityID int64, accountID string, confirm bool, note string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var reg models.Registration
		if err := tx.Where("activity_id = ? AND account_id = ?", activityID, accountID).First(&reg).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return ErrNotFound
			}
			return err
		}
		var win models.WinnerRecord
		winErr := tx.Where("activity_id = ? AND account_id = ?", activityID, accountID).First(&win).Error

		if confirm {
			if err := tx.Model(&reg).Updates(map[string]any{
				"status": models.RegEligible, "review_note": "复核通过：" + note,
			}).Error; err != nil {
				return err
			}
			if winErr == nil && win.Status == models.WinReviewing {
				// 恢复到中奖确认前/后的正确状态
				target := models.WinPending
				if win.PrizeItemID != nil {
					target = models.WinConfirmed
				}
				if err := tx.Model(&win).Update("status", target).Error; err != nil {
					return err
				}
			}
			auditTx(tx, activityID, "operator", "review.confirm", accountID+" 复核通过")
			return nil
		}

		// 判定失效
		if err := tx.Model(&reg).Updates(map[string]any{
			"status": models.RegIneligible, "review_note": "复核不通过：" + note,
		}).Error; err != nil {
			return err
		}
		if winErr == nil {
			switch win.Status {
			case models.WinReviewing, models.WinPending:
				if err := s.disqualifyWinner(tx, &win); err != nil {
					return err
				}
			case models.WinConfirmed:
				// 已占用库存但尚未发货：取消并释放编号，不补抽
				if err := s.disqualifyWinner(tx, &win); err != nil {
					return err
				}
			}
		}
		auditTx(tx, activityID, "operator", "review.reject", accountID+" 资格失效，名额不补抽")
		return nil
	})
}

// disqualifyWinner 取消中奖：若已占用奖品编号则释放回可发库存并记流水。
func (s *Service) disqualifyWinner(tx *gorm.DB, win *models.WinnerRecord) error {
	if win.PrizeItemID != nil {
		if err := tx.Model(&models.PrizeItem{}).Where("id = ?", *win.PrizeItemID).
			Updates(map[string]any{"status": models.ItemReleased, "winner_record_id": nil}).Error; err != nil {
			return err
		}
		if err := creditLedger(tx, win.PrizeID, 1, "release.disqualify", "winner", win.ID); err != nil {
			return err
		}
	}
	return tx.Model(win).Updates(map[string]any{
		"status": models.WinDisqualified, "prize_item_id": nil,
	}).Error
}

// ListRegistrations 运营查看报名名单
func (s *Service) ListRegistrations(activityID int64) ([]models.Registration, error) {
	var regs []models.Registration
	err := s.db.Where("activity_id = ?", activityID).Order("id asc").Find(&regs).Error
	return regs, err
}
