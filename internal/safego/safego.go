// Package safego runs long-lived background work with a recover boundary.
// A tray host can stay up for weeks: one panic in a retention or check-in loop
// must not take the whole gateway down, and it must be visible in the host log.
package safego

import (
	"fmt"
	"runtime/debug"
)

// Go runs fn in a new goroutine, logging any panic instead of letting it
// escape. It never restarts fn; the caller owns restart policy.
//
// log may be nil (skip logging) or any logger with Error/Warn/Info methods.
func Go(log any, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logPanic(log, name, r)
			}
		}()
		fn()
	}()
}

func logPanic(log any, name string, r any) {
	msg := "background task panicked: " + name
	fields := map[string]any{
		"panic": fmt.Sprint(r),
		"stack": string(debug.Stack()),
	}
	type errorLogger interface {
		Error(module, msg string, fields map[string]any)
	}
	type warnLogger interface {
		Warn(module, msg string, fields map[string]any)
	}
	type infoLogger interface {
		Info(module, msg string, fields map[string]any)
	}
	switch l := log.(type) {
	case errorLogger:
		l.Error("safego", msg, fields)
	case warnLogger:
		l.Warn("safego", msg, fields)
	case infoLogger:
		l.Info("safego", msg, fields)
	}
}
