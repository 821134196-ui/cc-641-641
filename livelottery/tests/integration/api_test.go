package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"livelottery/internal/api"
	"livelottery/internal/models"
	"livelottery/internal/service"
	"livelottery/internal/store"
)

func setupServer(t *testing.T) (*httptest.Server, *service.Service, int64, int64, []string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "it.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(db)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api.New(svc).Register(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	act, err := svc.CreateActivity(service.CreateActivityInput{
		Name: "it", DrawAtISO: time.Now().Add(time.Hour).Format(time.RFC3339),
		MinAccountAgeDays: 0, NeedBoundPhone: false,
		WinnerCount: 5, AddressHours: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	prize, err := svc.AddPrize(act.ID, "P", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LockActivity(act.ID); err != nil {
		t.Fatal(err)
	}
	accs := make([]string, 0, 9)
	for i := 0; i < 9; i++ {
		a := fmt.Sprintf("v%02d", i)
		accs = append(accs, a)
		if _, err := svc.Register(act.ID, service.ViewerProfile{AccountID: a, OrderNo: "O" + a}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.EndLive(act.ID); err != nil {
		t.Fatal(err)
	}
	return srv, svc, act.ID, prize.ID, accs
}

func postJSON(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	res, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// TestHTTP_ConcurrentConfirmNoOversell 经真实 HTTP 路由并发确认，不超扣。
func TestHTTP_ConcurrentConfirmNoOversell(t *testing.T) {
	srv, svc, actID, prizeID, _ := setupServer(t)
	if _, err := svc.Draw(actID); err != nil {
		t.Fatal(err)
	}
	res, err := svc.GetOfficialResult(actID)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var status2xx int64
	for _, w := range res.Winners {
		for k := 0; k < 8; k++ {
			wg.Add(1)
			go func(acc string) {
				defer wg.Done()
				code, _ := postJSON(t, fmt.Sprintf("%s/api/activities/%d/winners/%s/confirm", srv.URL, actID, acc),
					map[string]string{"receiver": acc, "phone": "13800000000", "address": "addr-" + acc})
				if code == 200 {
					atomic.AddInt64(&status2xx, 1)
				}
			}(w.AccountID)
		}
	}
	wg.Wait()

	items, err := svc.PrizeItems(prizeID)
	if err != nil {
		t.Fatal(err)
	}
	claimed := 0
	bound := map[int64]bool{}
	for _, it := range items {
		if it.Status == models.ItemClaimed {
			claimed++
			if bound[it.ID] {
				t.Fatalf("奖品编号重复绑定")
			}
			bound[it.ID] = true
		}
	}
	if claimed != 5 {
		t.Fatalf("HTTP 并发后占用=%d 期望5（超扣/空确认）", claimed)
	}
	// 重复确认走幂等分支也返回 200，因此 2xx 次数 ≥ 中奖人数属正常；
	// 真正不变量是占用实物恰好 5 件、无重复绑定（在下方断言）。
	if status2xx < 5 {
		t.Fatalf("HTTP 2xx 次数=%d 至少应5", status2xx)
	}
	t.Logf("并发确认 HTTP 2xx=%d（含幂等重试），实物占用唯一且=5", status2xx)
}

// TestHTTP_DrawIsUnique 并发 HTTP 开奖只有一次 200。
func TestHTTP_DrawIsUnique(t *testing.T) {
	srv, _, actID, _, _ := setupServer(t)
	var wg sync.WaitGroup
	var ok200 int64
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := http.Post(fmt.Sprintf("%s/api/activities/%d/draw", srv.URL, actID), "application/json", nil)
			if err != nil {
				t.Error(err)
			}
			res.Body.Close()
			if res.StatusCode == 200 {
				atomic.AddInt64(&ok200, 1)
			}
		}()
	}
	wg.Wait()
	if ok200 != 1 {
		t.Fatalf("HTTP 并发开奖 200 次数=%d 期望1", ok200)
	}
}

// TestHTTP_DuplicateReceiptIdempotent 重复回执经 HTTP 不产生重复发货。
func TestHTTP_DuplicateReceiptIdempotent(t *testing.T) {
	srv, svc, actID, _, _ := setupServer(t)
	draw, _ := svc.Draw(actID)
	w0 := draw.Winners[0]
	if _, err := svc.ConfirmWin(actID, w0.AccountID,
		service.AddressInput{Receiver: "r", Phone: "p", Address: "a"}); err != nil {
		t.Fatal(err)
	}
	w0Fresh := draw.Winners[0]
	_ = w0Fresh
	// 取 confirmed 记录 id
	off, _ := svc.GetOfficialResult(actID)
	var winID int64
	for _, w := range off.Winners {
		if w.AccountID == w0.AccountID {
			winID = w.ID
		}
	}
	if _, err := svc.CreateShipment(actID, winID, "TRK-9"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postJSON(t, srv.URL+"/api/logistics/receipt",
				map[string]string{"receipt_no": "R-DUP", "tracking_no": "TRK-9", "event_type": "delivered"})
		}()
	}
	wg.Wait()

	ships, err := svc.ListShipments(actID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ships) != 1 {
		t.Fatalf("重复回执后发货单数=%d 期望1", len(ships))
	}
	if ships[0].Status != models.ShipDelivered {
		t.Fatalf("发货单状态=%q 期望 delivered", ships[0].Status)
	}
}
