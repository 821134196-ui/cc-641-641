package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lottery/internal/models"
	"lottery/internal/service"
	"lottery/internal/sim"
)

type Handler struct {
	svc *service.Service
	sim *sim.Simulator
}

func New(svc *service.Service, simr *sim.Simulator) *Handler {
	return &Handler{svc: svc, sim: simr}
}

func (h *Handler) Register(r *gin.Engine) {
	api := r.Group("/api")
	{
		api.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

		api.POST("/campaigns", h.createCampaign)
		api.GET("/campaigns", h.listCampaigns)
		api.GET("/campaigns/:id", h.campaignDetail)
		api.POST("/campaigns/:id/lock", h.lock)
		api.POST("/campaigns/:id/open", h.openReg)
		api.POST("/campaigns/:id/register", h.register)
		api.POST("/campaigns/:id/close-verify", h.closeVerify)
		api.POST("/campaigns/:id/draw", h.draw)
		api.POST("/campaigns/:id/forfeit", h.forfeit)
		api.POST("/campaigns/:id/seed", h.seedWorld)
		api.GET("/campaigns/:id/entries", h.listEntries)
		api.GET("/campaigns/:id/orders", h.listOrders)
		api.GET("/campaigns/:id/reviews", h.listReviews)
		api.GET("/campaigns/:id/audit", h.listAudit)
		api.GET("/campaigns/:id/verify", h.verifyDraw)
		api.GET("/campaigns/:id/inventory", h.inventory)
		api.GET("/campaigns/:id/viewer", h.viewer)

		api.POST("/entries/:id/confirm", h.confirm)
		api.POST("/entries/:id/address", h.submitAddress)
		api.POST("/entries/:id/ship", h.ship)

		api.POST("/orders/:id/return", h.returnOrder)
		api.POST("/logistics/receipt", h.manualReceipt)

		api.GET("/accounts", h.listAccounts)
		api.GET("/sim/logs", h.simLogs)
	}
}

func fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrConflict), errors.Is(err, service.ErrAlreadyWinner),
		errors.Is(err, service.ErrDuplicateReg), errors.Is(err, service.ErrDuplicateDraw),
		errors.Is(err, service.ErrDeadlinePassed), errors.Is(err, service.ErrNoStock),
		errors.Is(err, service.ErrDuplicateSerial):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}

func id(c *gin.Context) uint {
	n, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	return uint(n)
}

// ---------- 活动 ----------

type createReq struct {
	Name            string              `json:"name"`
	MinActiveOrders int                 `json:"min_active_orders"`
	WinCount        int                 `json:"win_count"`
	AddressLimitMin int                 `json:"address_limit_minutes"`
	Prizes          []service.PrizeSpec `json:"prizes"`
}

func (h *Handler) createCampaign(c *gin.Context) {
	var req createReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	now := time.Now()
	cid, err := h.svc.CreateCampaign(req.Name, req.MinActiveOrders, req.WinCount, req.AddressLimitMin,
		req.Prizes, now, now.Add(2*time.Hour))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"id": cid})
}

func (h *Handler) listCampaigns(c *gin.Context) {
	var cs []models.Campaign
	h.svc.DB().Order("id DESC").Find(&cs)
	c.JSON(200, cs)
}

func (h *Handler) campaignDetail(c *gin.Context) {
	cid := id(c)
	out := gin.H{}
	var cmp models.Campaign
	if err := h.svc.DB().First(&cmp, cid).Error; err != nil {
		c.JSON(404, gin.H{"error": "活动不存在"})
		return
	}
	out["campaign"] = cmp
	var rule models.RuleSnapshot
	h.svc.DB().Where("campaign_id = ?", cid).First(&rule)
	out["rule"] = rule
	var prizes []models.Prize
	h.svc.DB().Where("campaign_id = ?", cid).Order("code ASC").Find(&prizes)
	out["prizes"] = prizes
	var regCnt, qualCnt int64
	h.svc.DB().Model(&models.Registration{}).Where("campaign_id = ?", cid).Count(&regCnt)
	h.svc.DB().Model(&models.Registration{}).Where("campaign_id = ? AND qualified = ?", cid, true).Count(&qualCnt)
	out["registration_count"] = regCnt
	out["qualified_count"] = qualCnt
	var batchCnt int64
	h.svc.DB().Model(&models.DrawBatch{}).Where("campaign_id = ?", cid).Count(&batchCnt)
	out["draw_batch_count"] = batchCnt
	statusCount := map[string]int{}
	rows, _ := h.svc.DB().Model(&models.DrawEntry{}).Where("campaign_id = ?", cid).
		Select("status, count(*) as cnt").Group("status").Rows()
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		rows.Scan(&st, &n)
		statusCount[st] = n
	}
	out["entry_status_count"] = statusCount
	c.JSON(200, out)
}

func (h *Handler) lock(c *gin.Context) {
	if err := h.svc.LockCampaign(id(c)); err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (h *Handler) openReg(c *gin.Context) {
	if err := h.svc.OpenRegistration(id(c)); err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

type registerReq struct {
	AccountID uint `json:"account_id"`
}

func (h *Handler) register(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	reg, err := h.svc.Register(id(c), req.AccountID)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, reg)
}

func (h *Handler) closeVerify(c *gin.Context) {
	q, d, err := h.svc.CloseAndVerify(id(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"qualified": q, "disqualified": d})
}

func (h *Handler) draw(c *gin.Context) {
	b, err := h.svc.Draw(id(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, b)
}

type forfeitReq struct {
	OffsetMinutes int `json:"offset_minutes"`
}

func (h *Handler) forfeit(c *gin.Context) {
	var req forfeitReq
	_ = c.ShouldBindJSON(&req)
	n := h.svc.ForfeitCampaign(id(c), time.Now().Add(time.Duration(req.OffsetMinutes)*time.Minute))
	c.JSON(200, gin.H{"forfeited": n})
}

type seedReq struct {
	Accounts   int `json:"accounts"`
	OrdersEach int `json:"orders_each"`
}

func (h *Handler) seedWorld(c *gin.Context) {
	var req seedReq
	_ = c.ShouldBindJSON(&req)
	if req.Accounts <= 0 {
		req.Accounts = 12
	}
	if req.Accounts > 50 {
		req.Accounts = 50
	}
	if err := h.sim.SeedWorld(id(c), req.Accounts, req.OrdersEach); err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true, "accounts": req.Accounts})
}

// ---------- 名单与发货 ----------

func decorateEntries(db *gorm.DB, entries []models.DrawEntry) []gin.H {
	out := make([]gin.H, 0, len(entries))
	for i := range entries {
		e := entries[i]
		row := gin.H{
			"id": e.ID, "rank": e.Rank, "account_id": e.AccountID, "prize_id": e.PrizeID,
			"status": e.Status, "receiver": e.Receiver, "phone": e.Phone, "address": e.Address,
			"forfeit_reason": e.ForfeitReason,
			"confirmed_at":   e.ConfirmedAt, "addressed_at": e.AddressedAt, "forfeited_at": e.ForfeitedAt,
		}
		var acc models.Account
		if db.First(&acc, e.AccountID).Error == nil {
			row["nickname"] = acc.Nickname
			row["unique_no"] = acc.UniqueNo
		}
		var pr models.Prize
		if db.First(&pr, e.PrizeID).Error == nil {
			row["prize_code"] = pr.Code
			row["prize_name"] = pr.Name
		}
		if e.PrizeUnitID != nil {
			var u models.PrizeUnit
			if db.First(&u, *e.PrizeUnitID).Error == nil {
				row["serial_no"] = u.SerialNo
			}
		}
		var sh models.Shipment
		if db.Where("entry_id = ?", e.ID).First(&sh).Error == nil {
			row["shipment_no"] = sh.ShipmentNo
			row["shipment_status"] = sh.Status
		}
		out = append(out, row)
	}
	return out
}

func (h *Handler) listEntries(c *gin.Context) {
	cid := id(c)
	var batch models.DrawBatch
	if err := h.svc.DB().Where("campaign_id = ?", cid).First(&batch).Error; err != nil {
		c.JSON(200, gin.H{"drawn": false, "entries": []any{}})
		return
	}
	var entries []models.DrawEntry
	h.svc.DB().Where("batch_id = ?", batch.ID).Order("rank ASC").Find(&entries)
	c.JSON(200, gin.H{
		"drawn": true, "batch": batch,
		"entries": decorateEntries(h.svc.DB().DB, entries),
	})
}

func (h *Handler) confirm(c *gin.Context) {
	e, err := h.svc.Confirm(parseEID(c))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true, "entry_id": e.ID, "prize_unit_id": e.PrizeUnitID})
}

type addressReq struct {
	Receiver string `json:"receiver"`
	Phone    string `json:"phone"`
	Address  string `json:"address"`
}

func (h *Handler) submitAddress(c *gin.Context) {
	var req addressReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.SubmitAddress(parseEID(c), req.Receiver, req.Phone, req.Address); err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (h *Handler) ship(c *gin.Context) {
	sh, err := h.svc.Ship(parseEID(c), "operator")
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, sh)
}

func parseEID(c *gin.Context) uint {
	n, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	return uint(n)
}

// ---------- 退货与回执 ----------

func (h *Handler) returnOrder(c *gin.Context) {
	oid, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	rc, err := h.svc.ReturnOrder(uint(oid))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, rc)
}

type receiptReq struct {
	ReceiptNo  string `json:"receipt_no"`
	ShipmentNo string `json:"shipment_no"`
	Event      string `json:"event"`
}

func (h *Handler) manualReceipt(c *gin.Context) {
	var req receiptReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.Event == "" {
		req.Event = "delivered"
	}
	dup, err := h.svc.ReceiveReceipt(req.ReceiptNo, req.ShipmentNo, req.Event)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"duplicate": dup})
}

// ---------- 查询 ----------

func (h *Handler) listOrders(c *gin.Context) {
	var os []models.Order
	h.svc.DB().Where("campaign_id = ?", id(c)).Order("id ASC").Find(&os)
	out := make([]gin.H, 0, len(os))
	for _, o := range os {
		var a models.Account
		h.svc.DB().First(&a, o.AccountID)
		out = append(out, gin.H{
			"id": o.ID, "account_id": o.AccountID, "account_name": a.Nickname,
			"sku": o.SKU, "amount_fen": o.AmountFen, "status": o.Status,
		})
	}
	c.JSON(200, gin.H{"orders": out})
}

func (h *Handler) listReviews(c *gin.Context) {
	var rs []models.ReviewCase
	h.svc.DB().Where("campaign_id = ?", id(c)).Order("id DESC").Find(&rs)
	c.JSON(200, rs)
}

func (h *Handler) listAudit(c *gin.Context) {
	var ls []models.AuditLog
	h.svc.DB().Where("campaign_id = ?", id(c)).Order("id ASC").Find(&ls)
	c.JSON(200, ls)
}

func (h *Handler) verifyDraw(c *gin.Context) {
	res, err := h.svc.VerifyDraw(id(c))
	if err != nil {
		c.JSON(404, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, res)
}

func (h *Handler) inventory(c *gin.Context) {
	res, err := h.svc.InventoryConsistent(id(c))
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, res)
}

func (h *Handler) listAccounts(c *gin.Context) {
	var as []models.Account
	h.svc.DB().Order("id ASC").Find(&as)
	out := make([]gin.H, 0, len(as))
	for _, a := range as {
		var activeOrders int64
		var cid uint
		if q := c.Query("campaign_id"); q != "" {
			n, _ := strconv.ParseUint(q, 10, 64)
			cid = uint(n)
		}
		if cid > 0 {
			h.svc.DB().Model(&models.Order{}).
				Where("campaign_id = ? AND account_id = ? AND status = ?", cid, a.ID, "active").
				Count(&activeOrders)
		}
		var reg *models.Registration
		if cid > 0 {
			var r models.Registration
			if h.svc.DB().Where("campaign_id = ? AND account_id = ?", cid, a.ID).First(&r).Error == nil {
				reg = &r
			}
		}
		out = append(out, gin.H{
			"id": a.ID, "nickname": a.Nickname, "unique_no": a.UniqueNo,
			"active_orders": activeOrders, "registration": reg,
		})
	}
	c.JSON(200, out)
}

func (h *Handler) viewer(c *gin.Context) {
	cid := id(c)
	accID, _ := strconv.ParseUint(c.Query("account_id"), 10, 64)
	out := gin.H{"campaign_id": cid}
	var cmp models.Campaign
	if err := h.svc.DB().First(&cmp, cid).Error; err != nil {
		c.JSON(404, gin.H{"error": "活动不存在"})
		return
	}
	out["campaign"] = cmp
	var rule models.RuleSnapshot
	h.svc.DB().Where("campaign_id = ?", cid).First(&rule)
	out["rule"] = rule
	if accID > 0 {
		var reg models.Registration
		if h.svc.DB().Where("campaign_id = ? AND account_id = ?", cid, accID).First(&reg).Error == nil {
			out["registration"] = reg
		}
		var entries []models.DrawEntry
		h.svc.DB().Where("campaign_id = ? AND account_id = ?", cid, accID).Find(&entries)
		out["entries"] = decorateEntries(h.svc.DB().DB, entries)
	}
	c.JSON(200, out)
}

func (h *Handler) simLogs(c *gin.Context) {
	c.JSON(200, gin.H{"logs": h.sim.Logs(), "inflight": h.svc.DB().Inflight()})
}
