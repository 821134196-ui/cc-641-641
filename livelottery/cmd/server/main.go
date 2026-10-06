package main

import (
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"livelottery/internal/api"
	"livelottery/internal/service"
	"livelottery/internal/store"
	"livelottery/web"
)

func main() {
	addr := env("ADDR", ":8080")
	dbPath := env("DB_PATH", "data/lottery.db")
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	db, err := store.Open(dbPath, os.Getenv("VERBOSE") == "1")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}

	svc := service.New(db)
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	h := api.New(svc)
	h.Register(r)

	// 静态前端（embed），SPA 路由回退到 index.html
	staticFS, err := fs.Sub(web.Static, "static")
	if err != nil {
		log.Fatal(err)
	}
	fileServer := http.FileServer(http.FS(staticFS))
	r.NoRoute(func(c *gin.Context) {
		p := strings.TrimPrefix(c.Request.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(staticFS, p); err != nil {
			// 非 API、非静态文件：SPA 回退
			if !strings.HasPrefix(c.Request.URL.Path, "/api/") {
				c.Request.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(c.Writer, c.Request)
	})

	log.Printf("直播抽奖系统已启动: http://localhost%s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatal(err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
