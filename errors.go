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

	// ErrTaskFailed means a master task exhausted its retries.
	ErrTaskFailed = errors.New("dnp3: task failed")

	// ErrFileTransfer means an outstation rejected a file operation. The
	// wrapped message carries the status code it reported.
	ErrFileTransfer = errors.New("dnp3: file transfer failed")

	// ErrNoConnection means the channel has no established connection.
	ErrNoConnection = errors.New("dnp3: no connection")
)
