package service

import (
	"fmt"

	"gorm.io/gorm"
	"livelottery/internal/models"
)

// CreateShipment 运营按中奖记录占用的奖品编号发货。
// 不变量：
//   - 仅 confirmed 可发货；
//   - 一个中奖记录至多一张发货单（winner_record_id 唯一）；
//   - 一个奖品编号至多出库一次（prize_item_id 唯一）；
//   - 发货同时把编号 claimed -> shipped，写流水。
func (s *Service) CreateShipment(activityID, winnerRecordID int64, trackingNo string) (*models.Shipment, error) {
	if trackingNo == "" {
		return nil, fmt.Errorf("物流单号必填")
	}
	var out *models.Shipment
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var win models.WinnerRecord
		if err := tx.First(&win, winnerRecordID).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return ErrNotFound
			}
			return err
		}
		if win.ActivityID != activityID {
			return ErrNotFound
		}
		if win.Status != models.WinConfirmed {
			return ErrWinnerNotPending
		}
		if win.PrizeItemID == nil {
			return ErrNoStock
		}

		ship := models.Shipment{
			WinnerRecordID: win.ID,
			PrizeItemID:    *win.PrizeItemID,
			TrackingNo:     trackingNo,
			Status:         models.ShipShipped,
		}
		shipped := s.now()
		ship.ShippedAt = &shipped
		if err := tx.Create(&ship).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrShipmentExists
			}
			return err
		}
		// 实物出库
		res := tx.Model(&models.PrizeItem{}).
			Where("id = ? AND status = ?", *win.PrizeItemID, models.ItemClaimed).
			Update("status", models.ItemShipped)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("奖品编号状态异常，拒绝发货")
		}
		if err := tx.Model(&win).Update("status", models.WinShipped).Error; err != nil {
			return err
		}
		// 出库不改变“可发库存”（确认时已扣），记 0 变动留痕；
		// 实际发货数量以 shipments 表为准，重复回执无法新增发货单。
		if err := writeLedger(tx, win.PrizeID, 0, mustAvail(tx, win.PrizeID), "ship.outbound", "shipment", ship.ID); err != nil {
			return err
		}
		auditTx(tx, activityID, "operator", "ship.create",
			fmt.Sprintf("奖品编号ID=%d 单号=%s 出库", *win.PrizeItemID, trackingNo))
		out = &ship
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReceiptEvent 模拟物流回执
type ReceiptEvent struct {
	ReceiptNo  string
	TrackingNo string // 关联发货单
	EventType  string // shipped / delivered
}

// HandleLogisticsReceipt 处理物流回执。
// 幂等关键：receipt_no 唯一索引 + 状态机，重复回执不会改变已确认的发货数量：
//   - 同一回执号再来：直接忽略（ErrDuplicateReceipt，调用方可视为成功）；
//   - 已 delivered 再收到 delivered/shipped：状态不回退、不重复记流水。
func (s *Service) HandleLogisticsReceipt(in ReceiptEvent) (dup bool, err error) {
	if in.ReceiptNo == "" || in.TrackingNo == "" {
		return false, fmt.Errorf("回执号与物流单号必填")
	}
	if in.EventType != "shipped" && in.EventType != "delivered" {
		return false, fmt.Errorf("未知回执事件类型: %s", in.EventType)
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var ship models.Shipment
		if err := tx.Where("tracking_no = ?", in.TrackingNo).First(&ship).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return ErrNotFound
			}
			return err
		}

		// 先尝试登记回执号；重复回执在此被唯一索引拦截，状态与数量完全不变
		rec := models.LogisticsReceipt{
			ReceiptNo:  in.ReceiptNo,
			ShipmentID: ship.ID,
			EventType:  in.EventType,
		}
		if err := tx.Create(&rec).Error; err != nil {
			if isUniqueViolation(err) {
				dup = true
				return ErrDuplicateReceipt
			}
			return err
		}

		// 状态机只前进、不重复
		if in.EventType == "delivered" && ship.Status != models.ShipDelivered {
			now := s.now()
			if err := tx.Model(&ship).Updates(map[string]any{
				"status": models.ShipDelivered, "delivered_at": now,
			}).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.WinnerRecord{}).Where("id = ?", ship.WinnerRecordID).
				Update("status", models.WinDelivered).Error; err != nil {
				return err
			}
			auditTx(tx, 0, "logistics", "receipt.delivered",
				fmt.Sprintf("单号=%s 回执=%s 首次签收", in.TrackingNo, in.ReceiptNo))
		}
		// shipped 回执或重复的 delivered 状态：发货数量早已在 CreateShipment 确定，此处不变
		return nil
	})
	if err == ErrDuplicateReceipt {
		return true, nil // 对调用方表现为幂等成功
	}
	return dup, err
}

// ListShipments / 库存流水 / 审计查询，供运营页与审计展示
func (s *Service) ListShipments(activityID int64) ([]models.Shipment, error) {
	var ships []models.Shipment
	err := s.db.
		Joins("JOIN winner_records ON winner_records.id = shipments.winner_record_id").
		Where("winner_records.activity_id = ?", activityID).
		Order("shipments.id asc").Find(&ships).Error
	return ships, err
}

func (s *Service) StockLedger(prizeID int64) ([]models.StockLedger, error) {
	var ls []models.StockLedger
	err := s.db.Where("prize_id = ?", prizeID).Order("id asc").Find(&ls).Error
	return ls, err
}

func (s *Service) AuditLogs(activityID int64) ([]models.AuditLog, error) {
	var ls []models.AuditLog
	err := s.db.Where("activity_id = ?", activityID).Order("id asc").Find(&ls).Error
	return ls, err
}

func (s *Service) PrizeItems(prizeID int64) ([]models.PrizeItem, error) {
	var items []models.PrizeItem
	err := s.db.Where("prize_id = ?", prizeID).Order("id asc").Find(&items).Error
	return items, err
}

func (s *Service) Prizes(activityID int64) ([]models.Prize, error) {
	var ps []models.Prize
	err := s.db.Where("activity_id = ?", activityID).Order("id asc").Find(&ps).Error
	return ps, err
}

// mustAvail 事务内读取当前可发库存余额（用于流水留痕）
func mustAvail(tx *gorm.DB, prizeID int64) int {
	var n int64
	tx.Model(&models.PrizeItem{}).
		Where("prize_id = ? AND status = ?", prizeID, models.ItemAvailable).
		Count(&n)
	return int(n)
}
