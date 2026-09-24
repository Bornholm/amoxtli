package gorm

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// Every connection of the pool gets the pragmas, and reads run in parallel.
func TestSQLitePoolAppliesPragmas(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "pool.sqlite"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %+v", errors.WithStack(err))
	}
	defer store.Close()

	db := store.DB()
	sqlDB, _ := db.DB()
	if sqlDB.Stats().MaxOpenConnections < 2 {
		t.Fatalf("expected a pool, got %d connections", sqlDB.Stats().MaxOpenConnections)
	}

	// Hold several connections at once, each checking its own pragmas.
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := db.WithContext(context.Background()).Connection(func(conn *gorm.DB) error {
				<-start
				var mode string
				var timeout int
				if err := conn.Raw("PRAGMA journal_mode").Scan(&mode).Error; err != nil {
					return err
				}
				if err := conn.Raw("PRAGMA busy_timeout").Scan(&timeout).Error; err != nil {
					return err
				}
				if mode != "wal" || timeout != 5000 {
					t.Errorf("unexpected pragmas: journal_mode=%s busy_timeout=%d", mode, timeout)
				}
				return nil
			})
			if err != nil {
				t.Errorf("Connection: %+v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
}
