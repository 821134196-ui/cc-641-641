package service

import (
	"fmt"
	"time"

	"gorm.io/gorm"
	"livelottery/internal/models"
)

// CreateActivityInput 创建活动时填写的规则（草稿态可反复修改）
type CreateActivityInput struct {
	Name      string `json:"name"`
	DrawAtISO string `json:"draw_at"` // 公示开奖时间

	MinAccountAgeDays int     `json:"min_account_age_days"`
	NeedBoundPhone    bool    `json:"need_bound_phone"`
	WinnerCount       int     `json:"winner_count"`
	AddressHours      float64 `json:"address_deadline_hours"`
}

// CreateActivity 建立草稿活动，规则此时尚未锁定。
func (s *Service) CreateActivity(in CreateActivityInput) (*models.Activity, error) {
	if in.Name == "" {
		return nil, fmt.Errorf("活动名称必填")
	}
	if in.WinnerCount <= 0 {
		return nil, fmt.Errorf("中奖人数必须为正")
	}
	if in.AddressHours <= 0 {
		return nil, fmt.Errorf("地址填写时限必须为正")
	}
	drawAt, err := parseTime(in.DrawAtISO)
	if err != nil {
		return nil, fmt.Errorf("开奖时间格式错误: %w", err)
	}

	act := &models.Activity{Name: in.Name, Status: models.StatusDraft, DrawAt: drawAt}
	rule := models.RuleSnapshot{
		MinAccountAgeDays:            in.MinAccountAgeDays,
		NeedBoundPhone:               in.NeedBoundPhone,
		WinnerCount:                  in.WinnerCount,
		AddressDeadlineHours:         in.AddressHours,
		OnAddressTimeout:             "forfeit",
		ReturnInvalidatesEligibility: true,
		Algorithm:                    "sha256-seeded-shuffle-v1",
	}
	// 草稿态先把规则存进 RuleJSON，便于预览；锁定时再次冻结覆盖。
	act.RuleJSON = rule.Encode()

	if err := s.db.Create(act).Error; err != nil {
		return nil, err
	}
	s.audit(act.ID, "operator", "activity.create", act.Name)
	return act, nil
}

// AddPrize 在草稿态登记一种奖品并写入其编号库存。
func (s *Service) AddPrize(activityID int64, name string, stock int) (*models.Prize, error) {
	if stock <= 0 {
		return nil, fmt.Errorf("库存必须为正")
	}
	var prize *models.Prize
	err := s.db.Transaction(func(tx *gorm.DB) error {
		act, err := getActivity(tx, activityID)
		if err != nil {
			return err
		}
		if act.Status != models.StatusDraft {
			return ErrRuleLocked
		}
		prize = &models.Prize{ActivityID: activityID, Name: name, TotalStock: stock}
		if err := tx.Create(prize).Error; err != nil {
			return err
		}
		items := make([]models.PrizeItem, 0, stock)
		for i := 1; i <= stock; i++ {
			items = append(items, models.PrizeItem{
				PrizeID: prize.ID, SerialNo: fmt.Sprintf("P%d-%04d", prize.ID, i),
				Status: models.ItemAvailable,
			})
		}
		if err := tx.CreateInBatches(items, 500).Error; err != nil {
			return err
		}
		return writeLedger(tx, prize.ID, stock, stock, "stock.init", "prize", prize.ID)
	})
	if err != nil {
		return nil, err
	}
	s.audit(activityID, "operator", "prize.add", fmt.Sprintf("%s x%d", name, stock))
	return prize, nil
}

// LockActivity 锁定参加条件、抽奖规则与奖品库存；锁定后进入报名阶段。
// 关键不变量：仅 draft 可锁定；必须存在不少于中奖人数的库存。
func (s *Service) LockActivity(activityID int64) (*models.Activity, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		act, err := getActivity(tx, activityID)
		if err != nil {
			return err
		}
		if act.Status != models.StatusDraft {
			if act.Status == models.StatusLocked {
				return ErrRuleLocked
			}
			return ErrInvalidState
		}
		rule, err := act.Rule()
		if err != nil {
			return err
		}
		var avail int64
		prizeID := s.firstPrizeID(tx, activityID)
		if err := tx.Model(&models.PrizeItem{}).
			Where("prize_id = ? AND status = ?", prizeID, models.ItemAvailable).
			Count(&avail).Error; err != nil {
			return err
		}
		if int(avail) < rule.WinnerCount {
			return fmt.Errorf("可用库存(%d)少于中奖人数(%d)，不能锁定", avail, rule.WinnerCount)
		}
		now := s.now()
		act.Status = models.StatusLocked
		act.LockedAt = &now
		// 重新冻结规则快照，此后不再变化
		rule.OnAddressTimeout = "forfeit"
		rule.ReturnInvalidatesEligibility = true
		if rule.Algorithm == "" {
			rule.Algorithm = "sha256-seeded-shuffle-v1"
		}
		act.RuleJSON = rule.Encode()
		return tx.Save(act).Error
	})
	if err != nil {
		return nil, err
	}
	s.audit(activityID, "operator", "activity.lock", "规则与库存冻结，开放报名")
	return s.GetActivity(activityID)
}

// EndLive 直播结束，停止报名，进入待开奖状态。
func (s *Service) EndLive(activityID int64) (*models.Activity, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		act, err := getActivity(tx, activityID)
		if err != nil {
			return err
		}
		if act.Status != models.StatusLocked {
			return ErrInvalidState
		}
		now := s.now()
		act.Status = models.StatusEnded
		act.EndedAt = &now
		return tx.Save(act).Error
	})
	if err != nil {
		return nil, err
	}
	s.audit(activityID, "operator", "live.end", "直播结束，停止报名")
	return s.GetActivity(activityID)
}

// GetActivity 查询活动（含规则反序列化供展示）
func (s *Service) GetActivity(id int64) (*models.Activity, error) {
	return getActivity(s.db, id)
}

// ListActivities 列出活动
func (s *Service) ListActivities() ([]models.Activity, error) {
	var acts []models.Activity
	if err := s.db.Order("id desc").Find(&acts).Error; err != nil {
		return nil, err
	}
	return acts, nil
}

func getActivity(tx *gorm.DB, id int64) (*models.Activity, error) {
	var act models.Activity
	if err := tx.First(&act, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &act, nil
}

func (s *Service) firstPrizeID(tx *gorm.DB, activityID int64) int64 {
	var p models.Prize
	tx.Where("activity_id = ?", activityID).Order("id asc").First(&p)
	return p.ID
}

// audit 写审计日志（失败不阻断主流程，但记录）
func (s *Service) audit(activityID int64, actor, action, detail string) {
	s.db.Create(&models.AuditLog{
		ActivityID: activityID, Actor: actor, Action: action, Detail: detail,
	})
}

// auditTx 在事务内写审计
func auditTx(tx *gorm.DB, activityID int64, actor, action, detail string) {
	tx.Create(&models.AuditLog{
		ActivityID: activityID, Actor: actor, Action: action, Detail: detail,
	})
}

func parseTime(iso string) (time.Time, error) {
	if iso == "" {
		return time.Time{}, fmt.Errorf("时间为空")
	}
	layouts := []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04:05"}
	var lastErr error
	for _, l := range layouts {
		if t, err := time.Parse(l, iso); err == nil {
			return t.UTC(), nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, lastErr
}
