// Package log is OpenDeezer's leveled logger (internal/log there), as the Deezer client in
// internal/deezer calls it, written to the add-on's own log (q2.SetupLog: /tmp, never the card).
// Debug lines are dropped unless Q2_DEBUG is set, so a playing track doesn't fill the log.
package log

import (
	"fmt"
	stdlog "log"
	"os"
)

// Level is a severity threshold.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	return [...]string{"DEBUG", "INFO", "WARN", "ERROR"}[l]
}

var level = func() Level {
	if os.Getenv("Q2_DEBUG") != "" {
		return LevelDebug
	}
	return LevelInfo
}()

func logf(l Level, format string, args ...any) {
	if l < level {
		return
	}
	stdlog.Printf("[%s] %s", l, fmt.Sprintf(format, args...))
}

// Debug / Info / Warn / Error emit at their level.
func Debug(format string, args ...any) { logf(LevelDebug, format, args...) }
func Info(format string, args ...any)  { logf(LevelInfo, format, args...) }
func Warn(format string, args ...any)  { logf(LevelWarn, format, args...) }
func Error(format string, args ...any) { logf(LevelError, format, args...) }
