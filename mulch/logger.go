package mulch

import "log"

// Logger is the log-output input interface for this package.
// Provide your own log interface, or use the DefaultLogger to simply wrap the log package.
// Leaving the interface nil disables all log output.
type Logger interface {
	// Debugf is used sparingly.
	Debugf(format string, v ...any)
	// Errorf is the most used of the loggers.
	Errorf(format string, v ...any)
	// Printf is only used when a custom Handler is not provided.
	Printf(format string, v ...any)
}

// DefaultLogger is a simple wrapper around the provided Logger interface.
// Use this if you only need simple log output.
type DefaultLogger struct {
	Silent bool
}

// Debugf prints a message with DEBUG prefixed.
func (l *DefaultLogger) Debugf(format string, v ...any) {
	if !l.Silent {
		log.Printf("[DEBUG] "+format, v...)
	}
}

// Errorf prints a message with ERROR prefixed.
func (l *DefaultLogger) Errorf(format string, v ...any) {
	if !l.Silent {
		log.Printf("[ERROR] "+format, v...)
	}
}

// Printf prints a message with INFO prefixed.
func (l *DefaultLogger) Printf(format string, v ...any) {
	if !l.Silent {
		log.Printf("[INFO] "+format, v...)
	}
}

// Validate DefaultLogger struct satisfies the Logger interface.
var _ = Logger(&DefaultLogger{})
