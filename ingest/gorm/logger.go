package gorm

import (
	"log"
	"os"
	"time"

	gormlogger "gorm.io/gorm/logger"
)

// newLogger returns the gorm logger shared by the store constructors: it only
// surfaces actual errors (record-not-found is expected on upsert lookups) and
// slow queries.
func newLogger() gormlogger.Interface {
	return gormlogger.New(
		log.New(os.Stderr, "\r\n", log.LstdFlags),
		gormlogger.Config{
			SlowThreshold:             time.Second,
			LogLevel:                  gormlogger.Error,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		},
	)
}
