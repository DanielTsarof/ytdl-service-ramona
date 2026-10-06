// Package logging builds the process-wide logger.
//
// Everything — this service and the libraries it uses — writes through the
// single *slog.Logger returned by Setup, so that a production run stays quiet
// at info while LOG_LEVEL=debug turns on the full diagnostic stream (yt-dlp
// resolves, ffmpeg argv and exit status, storage calls) without touching any
// code.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Setup builds the logger and installs it as slog.Default(), so libraries that
// fall back to the default logger land in the same stream and format.
//
// Unrecognised values fall back to the defaults and are reported through the
// logger itself — a typo in .env should be visible, not silently ignored.
func Setup(level, format string) *slog.Logger {
	lvl, levelOK := parseLevel(level)

	opts := &slog.HandlerOptions{
		Level: lvl,
		// Only on debug: file:line is exactly what you want while chasing a
		// bug and pure noise the rest of the time.
		AddSource: lvl <= slog.LevelDebug,
	}

	var handler slog.Handler
	formatOK := true
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	case "json":
		handler = slog.NewJSONHandler(os.Stdout, opts)
	default:
		handler = slog.NewTextHandler(os.Stdout, opts)
		formatOK = false
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)

	if !levelOK {
		logger.Warn("unknown LOG_LEVEL, falling back to info", slog.String("value", level))
	}
	if !formatOK {
		logger.Warn("unknown LOG_FORMAT, falling back to text", slog.String("value", format))
	}
	return logger
}

func parseLevel(level string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug, true
	case "", "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}
