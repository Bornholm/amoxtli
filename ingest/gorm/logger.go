package gorm

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/pkg/errors"
	gormlogger "gorm.io/gorm/logger"
)

// newLogger returns the gorm logger shared by the store constructors: it only
// surfaces actual errors (record-not-found is expected on upsert lookups,
// context cancellation is expected on shutdown) and slow queries.
func newLogger() gormlogger.Interface {
	return quietLogger{
		Interface: gormlogger.New(
			log.New(os.Stderr, "\r\n", log.LstdFlags),
			gormlogger.Config{
				SlowThreshold:             time.Second,
				LogLevel:                  gormlogger.Error,
				IgnoreRecordNotFoundError: true,
				Colorful:                  false,
			},
		),
	}
}

// quietLogger drops the traces of queries interrupted by a cancelled context.
//
// Closing a workspace cancels the context the background task runner polls
// with, so a poll in flight fails with context.Canceled and gorm logs it as an
// error — a stack of SQL noise printed on stderr *after* a command has
// succeeded, which reads like a failure. The cancellation is expected, and the
// caller receives the error anyway: only the log line is dropped.
//
// Deliberately narrow: context.DeadlineExceeded is left alone, since a query
// that outran its deadline is a real symptom worth seeing.
type quietLogger struct {
	gormlogger.Interface
}

// LogMode implements gormlogger.Interface, keeping the decoration across the
// level changes gorm performs internally (e.g. per-session loggers).
func (l quietLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	return quietLogger{Interface: l.Interface.LogMode(level)}
}

// Trace implements gormlogger.Interface.
func (l quietLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if errors.Is(err, context.Canceled) {
		return
	}

	l.Interface.Trace(ctx, begin, fc, err)
}

var _ gormlogger.Interface = quietLogger{}
