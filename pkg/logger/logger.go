package logger

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Interface defines the logging methods with structured key/value support.
type Interface interface {
	Debug(message string, fields ...interface{})
	Info(message string, fields ...interface{})
	Warn(message string, fields ...interface{})
	Error(message string, fields ...interface{})
	Fatal(message string, fields ...interface{})
}

// Logger wraps a zerolog.Logger and enriches logs with service metadata.
type Logger struct {
	logger zerolog.Logger
}

var _ Interface = (*Logger)(nil)

// New creates a configured Logger with timestamp, caller, hostname, pid, and service name.
// level: "debug", "info", "warn", "error"
// service: logical name of the application (e.g., "streetfood-customer-api").
func New(level, service string) *Logger {
	// Determine log level
	var lvl zerolog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = zerolog.DebugLevel
	case "info":
		lvl = zerolog.InfoLevel
	case "warn":
		lvl = zerolog.WarnLevel
	case "error":
		lvl = zerolog.ErrorLevel
	default:
		lvl = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(lvl)

	// Host and process metadata
	hostname, _ := os.Hostname()
	pid := strconv.Itoa(os.Getpid())

	// Console writer with timestamp format
	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
	}

	// Build logger
	logger := zerolog.New(writer).
		With().
		Timestamp().
		Str("service", service).
		Str("hostname", hostname).
		Str("pid", pid).
		Caller().
		Logger().Level(lvl)

	return &Logger{logger: logger}
}

// Debug logs a message at Debug level with optional fields.
func (l *Logger) Debug(message string, fields ...interface{}) {
	l.logWithLevel(zerolog.DebugLevel, message, fields...)
}

// Info logs a message at Info level with optional fields.
func (l *Logger) Info(message string, fields ...interface{}) {
	l.logWithLevel(zerolog.InfoLevel, message, fields...)
}

// Warn logs a message at Warn level with optional fields.
func (l *Logger) Warn(message string, fields ...interface{}) {
	l.logWithLevel(zerolog.WarnLevel, message, fields...)
}

// Error logs a message at Error level with optional fields.
func (l *Logger) Error(message string, fields ...interface{}) {
	l.logWithLevel(zerolog.ErrorLevel, message, fields...)
}

// Fatal logs a message at Fatal level with optional fields, then exits.
func (l *Logger) Fatal(message string, fields ...interface{}) {
	l.logWithLevel(zerolog.FatalLevel, message, fields...)
	os.Exit(1)
}

// logWithLevel builds a log event with structured fields (key/value pairs).
// fields should be provided as alternating key, value, ...
func (l *Logger) logWithLevel(level zerolog.Level, message string, fields ...interface{}) {
	event := l.logger.WithLevel(level)

	// inject structured fields
	for i := 0; i+1 < len(fields); i += 2 {
		key := fmt.Sprint(fields[i])
		event = event.Interface(key, fields[i+1])
	}

	// log the message
	event.Timestamp().Caller().Msg(message)
}
