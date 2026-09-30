package dnp3

import "errors"

// Errors returned across the stack. Layer packages define their own detailed
// errors and wrap them in these, so callers can classify a failure without
// importing internal packages.
var (
	// ErrMalformed means a received octet sequence did not conform to the
	// protocol and could not be recovered.
	ErrMalformed = errors.New("dnp3: malformed data")

	// ErrTimeout means a peer did not respond within the configured window.
	ErrTimeout = errors.New("dnp3: timeout")

	// ErrClosed means the session or channel has been shut down.
	ErrClosed = errors.New("dnp3: closed")

	// ErrNotSupported means the peer rejected a function code or object it
	// does not implement.
	ErrNotSupported = errors.New("dnp3: not supported by peer")

	// ErrBadConfig means a configuration value is outside the range the
	// protocol allows.
	ErrBadConfig = errors.New("dnp3: invalid configuration")

	// ErrRejected means the outstation answered a request but set an
	// indication saying it refused it: PARAMETER_ERROR or OBJECT_UNKNOWN.
	ErrRejected = errors.New("dnp3: request rejected by peer")

	// ErrTaskFailed means a master task was dropped before it ran, for example
	// because the outstation restarted while it was queued. The master does not
	// retry tasks at the application layer.
	ErrTaskFailed = errors.New("dnp3: task failed")

	// ErrFileTransfer means an outstation rejected a file operation. The
	// wrapped message carries the status code it reported.
	ErrFileTransfer = errors.New("dnp3: file transfer failed")

	// ErrNoConnection means the channel has no established connection.
	ErrNoConnection = errors.New("dnp3: no connection")
)
