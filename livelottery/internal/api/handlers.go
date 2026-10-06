package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"livelottery/internal/service"
)

type Handler struct{ svc *service.Service }

func New(svc *service.Service) *Handler { return &Handler{svc: svc} }

// Register 挂载全部路由
func (h *Handler) Register(r *gin.Engine) {
	api := r.Group("/api")
	{
		api.GET("/health", h.health)

		acts := api.Group("/activities")
		{
			acts.GET("", h.listActivities)
			acts.POST("", h.createActivity)
			acts.GET("/:id", h.getActivity)
			acts.POST("/:id/prizes", h.addPrize)
			acts.POST("/:id/lock", h.lockActivity)
			acts.POST("/:id/end", h.endLive)
			acts.POST("/:id/draw", h.draw)
			acts.GET("/:id/result", h.getResult)
			acts.POST("/:id/verify-hash", h.verifyHash)
			acts.GET("/:id/registrations", h.listRegistrations)
			acts.GET("/:id/shipments", h.listShipments)
			acts.GET("/:id/audit", h.auditLogs)
			acts.GET("/:id/prizes", h.listPrizes)

			acts.POST("/:id/register", h.register)
			acts.POST("/:id/return", h.returnOrder)
			acts.POST("/:id/review", h.reviewDecision)
			acts.POST("/:id/expire", h.expire)
			acts.POST("/:id/sim/return-order", h.simReturnOrder)

			acts.POST("/:id/winners/:account/confirm", h.confirmWin)
			acts.GET("/:id/viewer/:account", h.viewerStatus)
			acts.POST("/:id/ship", h.createShipment)
		}

		api.POST("/logistics/receipt", h.logisticsReceipt)
		api.GET("/prizes/:pid/ledger", h.stockLedger)
		api.GET("/prizes/:pid/items", h.prizeItems)

		api.POST("/sim/demo", h.simDemo)
	}
}

func (h *Handler) health(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) }

func (h *Handler) simDemo(c *gin.Context) {
	res, err := h.svc.RunDemo()
	ok(c, res, err)
}

// ---- 活动 ----

func (h *Handler) listActivities(c *gin.Context) {
	acts, err := h.svc.ListActivities()
	ok(c, acts, err)
}

func (h *Handler) createActivity(c *gin.Context) {
	var in service.CreateActivityInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	act, err := h.svc.CreateActivity(in)
	ok(c, act, err)
}

func (h *Handler) getActivity(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	act, err := h.svc.GetActivity(id)
	ok(c, act, err)
}

type prizeReq struct {
	Name  string `json:"name"`
	Stock int    `json:"stock"`
}

func (h *Handler) addPrize(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var in prizeReq
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	p, err := h.svc.AddPrize(id, in.Name, in.Stock)
	ok(c, p, err)
}

func (h *Handler) lockActivity(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	act, err := h.svc.LockActivity(id)
	ok(c, act, err)
}

func (h *Handler) endLive(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	act, err := h.svc.EndLive(id)
	ok(c, act, err)
}

// ---- 报名 / 退货 / 复核 ----

type registerReq struct {
	AccountID      string `json:"account_id"`
	OrderNo        string `json:"order_no"`
	AccountAgeDays int    `json:"account_age_days"`
	PhoneBound     bool   `json:"phone_bound"`
}

func (h *Handler) register(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var in registerReq
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	reg, err := h.svc.Register(id, service.ViewerProfile{
		AccountID: in.AccountID, OrderNo: in.OrderNo,
		AccountAgeDays: in.AccountAgeDays, PhoneBound: in.PhoneBound,
	})
	ok(c, reg, err)
}

func (h *Handler) listRegistrations(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	regs, err := h.svc.ListRegistrations(id)
	ok(c, regs, err)
}

type returnReq struct {
	AccountID string `json:"account_id"`
	OrderNo   string `json:"order_no"`
}

func (h *Handler) returnOrder(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var in returnReq
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	ok(c, gin.H{"status": "in_review"}, h.svc.ReturnOrder(id, in.AccountID, in.OrderNo))
}

// simReturnOrder 本地模拟“直播订单退货”事件（账号/订单由服务端造数）
func (h *Handler) simReturnOrder(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var in returnReq
	_ = c.ShouldBindJSON(&in)
	if in.OrderNo == "" {
		in.OrderNo = "ORD-SIM-" + in.AccountID
	}
	ok(c, gin.H{"status": "in_review"}, h.svc.ReturnOrder(id, in.AccountID, in.OrderNo))
}

type reviewReq struct {
	AccountID string `json:"account_id"`
	Confirm   bool   `json:"confirm"`
	Note      string `json:"note"`
}

func (h *Handler) reviewDecision(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var in reviewReq
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	ok(c, gin.H{"done": true}, h.svc.ReviewDecision(id, in.AccountID, in.Confirm, in.Note))
}

// ---- 开奖 ----

func (h *Handler) draw(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	res, err := h.svc.Draw(id)
	ok(c, res, err)
}

func (h *Handler) getResult(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	res, err := h.svc.GetOfficialResult(id)
	ok(c, res, err)
}

func (h *Handler) verifyHash(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	match, got, err := h.svc.VerifyResultHash(id)
	ok(c, gin.H{"hash_matches": match, "recomputed": got}, err)
}

// ---- 中奖确认 / 超时 ----

type confirmReq struct {
	Receiver string `json:"receiver"`
	Phone    string `json:"phone"`
	Address  string `json:"address"`
}

func (h *Handler) confirmWin(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	acc := c.Param("account")
	var in confirmReq
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	w, err := h.svc.ConfirmWin(id, acc, service.AddressInput{
		Receiver: in.Receiver, Phone: in.Phone, Address: in.Address,
	})
	ok(c, w, err)
}

func (h *Handler) expire(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	n, err := h.svc.ExpireOverdueWinners(id)
	ok(c, gin.H{"expired": n}, err)
}

func (h *Handler) viewerStatus(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	acc := c.Param("account")
	st, err := h.svc.ViewerStatus(id, acc)
	ok(c, st, err)
}

// ---- 发货 / 物流 ----

type shipReq struct {
	WinnerRecordID int64  `json:"winner_record_id"`
	TrackingNo     string `json:"tracking_no"`
}

func (h *Handler) createShipment(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var in shipReq
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	s, err := h.svc.CreateShipment(id, in.WinnerRecordID, in.TrackingNo)
	ok(c, s, err)
}

type receiptReq struct {
	ReceiptNo  string `json:"receipt_no"`
	TrackingNo string `json:"tracking_no"`
	EventType  string `json:"event_type"`
}

func (h *Handler) logisticsReceipt(c *gin.Context) {
	var in receiptReq
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	dup, err := h.svc.HandleLogisticsReceipt(service.ReceiptEvent{
		ReceiptNo: in.ReceiptNo, TrackingNo: in.TrackingNo, EventType: in.EventType,
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(200, gin.H{"accepted": true, "duplicate": dup})
}

func (h *Handler) listShipments(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	s, err := h.svc.ListShipments(id)
	ok(c, s, err)
}

func (h *Handler) stockLedger(c *gin.Context) {
	pid, _ := strconv.ParseInt(c.Param("pid"), 10, 64)
	l, err := h.svc.StockLedger(pid)
	ok(c, l, err)
}

func (h *Handler) prizeItems(c *gin.Context) {
	pid, _ := strconv.ParseInt(c.Param("pid"), 10, 64)
	items, err := h.svc.PrizeItems(pid)
	ok(c, items, err)
}

func (h *Handler) auditLogs(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	l, err := h.svc.AuditLogs(id)
	ok(c, l, err)
}

func (h *Handler) listPrizes(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	ps, err := h.svc.Prizes(id)
	ok(c, ps, err)
}

// ---- 统一响应 / 错误映射 ----

func ok(c *gin.Context, data any, err error) {
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(200, gin.H{"data": data})
}

func writeErr(c *gin.Context, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, service.ErrNotFound):
		code = 404
	case errors.Is(err, service.ErrAlreadyDrawn),
		errors.Is(err, service.ErrAlreadyRegistered),
		errors.Is(err, service.ErrShipmentExists):
		code = 409
	case errors.Is(err, service.ErrRuleLocked),
		errors.Is(err, service.ErrInvalidState),
		errors.Is(err, service.ErrRegistrationClosed):
		code = 409
	case errors.Is(err, service.ErrNoStock):
		code = 409
	case errors.Is(err, service.ErrAddressTimeout):
		code = 422
	default:
		code = 400
	}
	c.JSON(code, gin.H{"error": err.Error()})
}
