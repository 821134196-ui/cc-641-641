package service

import (
	"errors"
	"strings"

	"gorm.io/gorm"
	"livelottery/internal/models"
)

// ---- 库存流水 ----
// 可发库存不单独维护计数器，而由 PrizeItem.status 聚合得出，避免“计数与实物不一致”。
// 流水仅作审计；任何时刻 available 数量可用 status='available' 重算并与流水余额对账。

func writeLedger(tx *gorm.DB, prizeID int64, change, balanceAfter int, reason, refType string, refID int64) error {
	return tx.Create(&models.StockLedger{
		PrizeID: prizeID, Change: change, BalanceAfter: balanceAfter,
		Reason: reason, RefType: refType, RefID: refID,
	}).Error
}

func availableCount(tx *gorm.DB, prizeID int64) (int, error) {
	var n int64
	if err := tx.Model(&models.PrizeItem{}).
		Where("prize_id = ? AND status = ?", prizeID, models.ItemAvailable).
		Count(&n).Error; err != nil {
		return 0, err
	}
	return int(n), nil
}

// debitLedger 扣减后记录流水（余额=扣减后 available 实物数）
func debitLedger(tx *gorm.DB, prizeID int64, reason string, refID int64) error {
	n, err := availableCount(tx, prizeID)
	if err != nil {
		return err
	}
	return writeLedger(tx, prizeID, -1, n, reason, "winner", refID)
}

// creditLedger 释放后记录流水（余额=释放后 available 实物数）
func creditLedger(tx *gorm.DB, prizeID int64, change int, reason, refType string, refID int64) error {
	n, err := availableCount(tx, prizeID)
	if err != nil {
		return err
	}
	return writeLedger(tx, prizeID, change, n, reason, refType, refID)
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, gorm.ErrDuplicatedKey) ||
		strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "Duplicate entry") ||
		strings.Contains(err.Error(), "duplicate key")
}
