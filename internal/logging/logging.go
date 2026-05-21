package logging

import (
	"io"
	"log/slog"
	"os"
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
	var h slog.Handler
	if os.Getenv("DEVELOPMENT") == "true" {
		h = tint.NewHandler(os.Stderr, &tint.Options{
			TimeFormat: time.Kitchen,
			Level:      slog.LevelDebug,
		})
	} else {
		h = slog.NewJSONHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(h))
}
