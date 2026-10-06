package store

import (
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"livelottery/internal/models"
)

// Open 打开/创建 SQLite 数据库。
//
// 并发正确性关键设置：
//   - journal_mode=WAL：读写不互相阻塞；
//   - busy_timeout：写锁竞争时等待而不是立即报 SQLITE_BUSY；
//   - _txlock=immediate：事务一开始即拿写锁，把“扣库存”这类
//     读改写操作串行化，从数据库层杜绝超扣；
//   - foreign_keys=on：外键约束生效。
func Open(path string, verbose bool) (*gorm.DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(8000)"+
			"&_pragma=foreign_keys(ON)&_txlock=immediate",
		path,
	)
	logLevel := logger.Silent
	if verbose {
		logLevel = logger.Warn
	}
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:  logger.Default.LogMode(logLevel),
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	// 连接池：WAL 下允许并发读，写事务被 immediate 锁串行化。
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if err := db.AutoMigrate(models.AllModels()...); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}
