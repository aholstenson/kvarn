package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/lmittmann/tint"
)

// SetupForWriter configures the default slog logger to write to the given
// writer. Useful in tests to redirect log output to GinkgoWriter so that
// it is only shown on failure.
func SetupForWriter(w io.Writer) {
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
}

// Setup configures the default slog logger. When the DEVELOPMENT environment
// variable is set to "true", a colorized text handler is used. Otherwise, a
// structured JSON handler is used.
func Setup() {
	level := Level()
	var h slog.Handler
	if os.Getenv("DEVELOPMENT") == "true" {
		h = tint.NewHandler(os.Stderr, &tint.Options{
			TimeFormat: time.Kitchen,
			Level:      level,
		})
	} else {
		h = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}
	slog.SetDefault(slog.New(h))
}

// Level resolves the minimum level to log from KVARN_LOG_LEVEL. Subsystems that
// do routine, high-frequency work — cloning, mirror refreshes, transfers — keep
// their per-step detail at debug so a production log stays readable; this is the
// one knob that turns that detail on without a rebuild.
//
// Development runs default to debug because that detail is the point of them.
func Level() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KVARN_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	if os.Getenv("DEVELOPMENT") == "true" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// Elapsed renders how long an operation took, for use as a log field value.
// slog writes a time.Duration as a bare nanosecond count, which is unreadable
// at a glance; "1.204s" reads the same way in every handler.
func Elapsed(start time.Time) string {
	return time.Since(start).Round(time.Millisecond).String()
}
