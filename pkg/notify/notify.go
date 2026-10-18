// Package notify sends operator notifications: trade fills, start-up, login
// prompts, square-off and failures.
//
// It mirrors tradekit's harness notifier (used by rsrankbot) rather than
// importing it: tradekit resolves from a sibling checkout through replace
// directives, which the Docker build context cannot see.
package notify

import "fmt"

// Level is how urgent a notification is.
type Level int

// Info is routine, Warn needs a look, Alert needs a human now.
const (
	Info Level = iota
	Warn
	Alert
)

func (l Level) String() string {
	switch l {
	case Info:
		return "info"
	case Warn:
		return "warn"
	case Alert:
		return "alert"
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// Notifier sends a message.
//
// Implementations must not block and return nothing on purpose: an error
// return invites `if err != nil { return }` in the execution path, and then a
// Telegram outage cancels an order. Failures are logged by the implementation.
type Notifier interface {
	Notify(level Level, msg string)
}

// Discard drops everything. The default when Telegram is not configured.
type Discard struct{}

// Notify does nothing.
func (Discard) Notify(Level, string) {}

// Or returns n, or Discard when n is nil, so callers never nil-check.
func Or(n Notifier) Notifier {
	if n == nil {
		return Discard{}
	}
	return n
}
