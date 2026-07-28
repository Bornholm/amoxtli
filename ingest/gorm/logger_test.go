package gorm

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/pkg/errors"
	gormlogger "gorm.io/gorm/logger"
)

// TestQuietLoggerDropsCancellation locks the shutdown behaviour: closing a
// workspace cancels the context the task runner polls with, and the resulting
// trace must not be printed after a command has already succeeded.
func TestQuietLoggerDropsCancellation(t *testing.T) {
	testCases := []struct {
		Name     string
		Err      error
		Expected bool
	}{
		{"cancelled", context.Canceled, false},
		{"wrapped cancellation", errors.Wrap(context.Canceled, "querying tasks"), false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"real error", errors.New("no such table"), true},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			var buf bytes.Buffer

			logger := quietLogger{
				Interface: gormlogger.New(
					log.New(&buf, "", 0),
					gormlogger.Config{LogLevel: gormlogger.Error},
				),
			}

			logger.Trace(context.Background(), time.Now(),
				func() (string, int64) { return "SELECT 1", 0 },
				tc.Err,
			)

			if logged := buf.Len() > 0; logged != tc.Expected {
				t.Errorf("logged = %v, expected %v (output: %q)", logged, tc.Expected, buf.String())
			}
		})
	}
}

// TestQuietLoggerSurvivesLogMode covers the decoration gorm rebuilds when it
// derives a session logger: losing it there would bring the noise back.
func TestQuietLoggerSurvivesLogMode(t *testing.T) {
	var buf bytes.Buffer

	logger := quietLogger{
		Interface: gormlogger.New(
			log.New(&buf, "", 0),
			gormlogger.Config{LogLevel: gormlogger.Error},
		),
	}

	derived := logger.LogMode(gormlogger.Error)

	if _, ok := derived.(quietLogger); !ok {
		t.Fatalf("LogMode must keep the decoration, got %T", derived)
	}

	derived.Trace(context.Background(), time.Now(),
		func() (string, int64) { return "SELECT 1", 0 },
		context.Canceled,
	)

	if buf.Len() > 0 {
		t.Errorf("expected no output, got %q", buf.String())
	}

	// A real error still goes through the derived logger.
	derived.Trace(context.Background(), time.Now(),
		func() (string, int64) { return "SELECT 1", 0 },
		errors.New("boom"),
	)

	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("expected the real error to be logged, got %q", buf.String())
	}

}
