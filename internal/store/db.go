package store

import (
	"database/sql"
	"errors"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var ErrNotFound = errors.New("not found")

// DB 包装 GORM。inflight 用于页面展示当前正在处理的写入数。
type DB struct {
	*gorm.DB
	inflight atomic.Int64
}

func Open(path string) (*DB, error) {
	// _txlock=immediate：事务一开始即获取 SQLite 写锁，配合 busy_timeout
	// 让并发写入串行排队，而不是在 COMMIT 时撞上 SQLITE_BUSY。
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_txlock=immediate"
	// 业务冲突（唯一约束、查无此行）都以 error 返回由业务层判定，静默 GORM 的默认错误打印；
	// 仅保留慢查询追踪。
	glog := logger.New(log.New(os.Stdout, "", log.LstdFlags), logger.Config{
		SlowThreshold:             500 * time.Millisecond,
		LogLevel:                  logger.Silent,
		IgnoreRecordNotFoundError: true,
	})
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: glog})
	if err != nil {
		return nil, err
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(8)
	return &DB{DB: gdb}, nil
}

// Tx 在 IMMEDIATE 事务中执行 fn；返回 nil 提交，error 回滚。
func (d *DB) Tx(fn func(tx *gorm.DB) error) error {
	d.inflight.Add(1)
	defer d.inflight.Add(-1)
	return d.DB.Transaction(fn, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}

func (d *DB) Inflight() int64 { return d.inflight.Load() }
