// Package secevent reports security-relevant events observed by DNP3 masters,
// outstations and channels.
//
// A session configured with a [Policy] emits typed [Event]s — an unexpected
// connection, a TLS certificate refused, a malformed frame, a control executed
// or refused, a restart, an authentication failure, a flood — to a [Sink] the
// application supplies. The events are a separate stream from the session's
// slog output: each has a stable [Kind], a [Category], a [Severity] and the
// identity of the peer it came from, so it can be forwarded to a SIEM without
// parsing log lines.
//
// Delivery never blocks a protocol goroutine. Events go through a
// [Dispatcher], which queues them, delivers them on its own goroutine,
// aggregates repeats, and counts what it had to drop. A session without a
// Policy pays one nil check per would-be event.
//
// The library reports what it observes as a protocol endpoint. It is not an
// intrusion detection system: correlating events across devices belongs to
// whatever consumes them.
package secevent
