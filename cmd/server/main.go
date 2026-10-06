package main

import (
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lottery/internal/api"
	"lottery/internal/models"
	"lottery/internal/service"
	"lottery/internal/sim"
	"lottery/internal/store"
	"lottery/web"
)

func main() {
	dbPath := flag.String("db", "data/lottery.db", "SQLite 数据库文件路径")
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	flag.Parse()

	if err := os.MkdirAll("data", 0o755); err != nil {
		log.Fatal(err)
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	if err := db.AutoMigrate(models.AllModels()...); err != nil {
		log.Fatalf("迁移失败: %v", err)
	}

	svc := service.New(db)
	simr := sim.New(svc)
	bootstrapDemo(svc, simr)

	// 后台本地事件模拟：直播报名流量 + 物流妥投回执（含周期性重复回执）
	stop := make(chan struct{})
	go simr.RunLogistics(stop)
	go runTrafficWatch(svc, simr, stop)
	// 真实时间巡检：按锁定规则处理超期未填地址
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			if n := svc.ForfeitOverdue(time.Now()); n > 0 {
				log.Printf("[forfeit] 超期放弃 %d 人（按锁定规则，不补抽）", n)
			}
		}
	}()

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	api.New(svc, simr).Register(r)

	static, err := fs.Sub(web.Files, ".")
	if err != nil {
		log.Fatal(err)
	}
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if len(p) >= 4 && p[:4] == "/api" {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		// SPA 回退：未命中静态文件时返回 index.html
		if _, err := fs.Stat(static, strings.TrimPrefix(p, "/")); err != nil {
			c.Request.URL.Path = "/"
		}
		c.FileFromFS(c.Request.URL.Path, http.FS(static))
	})

	log.Printf("电商直播抽奖系统已启动：http://localhost%s （数据文件 %s）", *addr, *dbPath)
	if err := r.Run(*addr); err != nil {
		log.Fatal(err)
	}
}

// runTrafficWatch 当存在 open 状态活动时运行报名流量模拟，全部结束后协程常驻等待。
func runTrafficWatch(svc *service.Service, simr *sim.Simulator, stop <-chan struct{}) {
	started := map[uint]bool{}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	innerStop := make(chan struct{})
	for {
		select {
		case <-stop:
			close(innerStop)
			return
		case <-t.C:
			var cs []models.Campaign
			svc.DB().Where("status = ?", models.StatusOpen).Find(&cs)
			for _, c := range cs {
				if !started[c.ID] {
					started[c.ID] = true
					go simr.RunLiveTraffic(c.ID, innerStop)
				}
			}
		}
	}
}

// bootstrapDemo 首次启动时准备一个待锁定的演示活动和模拟观众/订单。
func bootstrapDemo(svc *service.Service, simr *sim.Simulator) {
	var cnt int64
	svc.DB().Model(&models.Campaign{}).Count(&cnt)
	if cnt > 0 {
		return
	}
	cid, err := svc.CreateCampaign(
		"秋季新品直播专场",
		2,  // 锁定条件：活动期间有效订单 >= 2 单
		5,  // 正式中奖 5 人
		30, // 锁定规则：开奖后 30 分钟内填地址，超期放弃不补抽
		[]service.PrizeSpec{
			{Code: "A", Name: "旗舰手机", Stock: 1},
			{Code: "B", Name: "蓝牙耳机", Stock: 2},
			{Code: "C", Name: "定制周边礼盒", Stock: 2},
		},
		time.Now(), time.Now().Add(2*time.Hour),
	)
	if err != nil {
		log.Printf("引导演示活动失败: %v", err)
		return
	}
	if err := simr.SeedWorld(cid, 12, 2); err != nil {
		log.Printf("引导模拟观众失败: %v", err)
		return
	}
	log.Printf("已引导演示活动 #%d（草稿待锁定）与 15 个模拟账号", cid)
}
