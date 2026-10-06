package service

import (
	"gorm.io/gorm"
	"livelottery/internal/models"
)

// ViewerStatus 观众页所需的全部信息：公示开奖时间、规则、报名状态、中奖与发货状态
type ViewerStatus struct {
	Activity     *models.Activity     `json:"activity"`
	Registration *models.Registration `json:"registration,omitempty"`
	Winner       *models.WinnerRecord `json:"winner,omitempty"`
	Shipment     *models.Shipment     `json:"shipment,omitempty"`
	Drawn        bool                 `json:"drawn"`
	ResultHash   string               `json:"result_hash,omitempty"`
}

// ViewerStatus 聚合观众视角数据
func (s *Service) ViewerStatus(activityID int64, accountID string) (*ViewerStatus, error) {
	var act models.Activity
	if err := s.db.First(&act, activityID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	st := &ViewerStatus{Activity: &act, Drawn: act.Status == models.StatusDrawn}

	var reg models.Registration
	if err := s.db.Where("activity_id = ? AND account_id = ?", activityID, accountID).First(&reg).Error; err == nil {
		st.Registration = &reg
	}

	var win models.WinnerRecord
	if err := s.db.Where("activity_id = ? AND account_id = ?", activityID, accountID).First(&win).Error; err == nil {
		st.Winner = &win
		var ship models.Shipment
		if err := s.db.Where("winner_record_id = ?", win.ID).First(&ship).Error; err == nil {
			st.Shipment = &ship
		}
	}

	var batch models.DrawBatch
	if err := s.db.Where("activity_id = ?", activityID).First(&batch).Error; err == nil {
		st.ResultHash = batch.ResultHash
	}
	return st, nil
}
