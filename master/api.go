package master

import (
	"context"
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
)

// The request methods below are safe to call from any goroutine. Each hands a
// task to the session goroutine and waits for it to finish, so no caller ever
// touches protocol state directly.

// run submits a task and waits for its outcome.
//
// The caller keeps its own reference to the completion channel rather than
// reading it back off the task. Once the task is submitted it belongs to the
// session goroutine, which clears the field when it finishes — reading t.done
// here afterwards would be a race on a field the session owns.
func (s *Session) run(ctx context.Context, t *task) error {
	done := make(chan error, 1)
	t.done = done

	select {
	case s.submit <- t:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ScanClasses reads the given classes once and waits for the response.
//
// An integrity poll is [dnp3.ClassAll]; a routine event poll is
// [dnp3.Class123].
func (s *Session) ScanClasses(ctx context.Context, mask dnp3.Class) error {
	if mask == 0 {
		return fmt.Errorf("master: %w: empty class mask", dnp3.ErrBadConfig)
	}
	return s.run(ctx, newScanTask(mask, s.handler))
}

// IntegrityPoll reads every class, which re-baselines the master's picture of
// the outstation.
func (s *Session) IntegrityPoll(ctx context.Context) error {
	return s.ScanClasses(ctx, dnp3.ClassAll)
}

// ScanRange reads a contiguous index range of one group and variation.
//
// Pass variation zero to let the outstation choose its default, which is what
// a master normally wants: the outstation knows which encoding carries its
// points without loss.
func (s *Session) ScanRange(ctx context.Context, group, variation uint8, start, stop uint16) error {
	if start > stop {
		return fmt.Errorf("master: %w: start %d above stop %d", dnp3.ErrBadConfig, start, stop)
	}
	return s.run(ctx, newRangeScanTask(group, variation, start, stop))
}

// AddPeriodicScan schedules a recurring class poll.
//
// It returns as soon as the scan is queued; the poll then runs for the life of
// the session. Failures do not stop it — a poll that fails because the link
// dropped must keep trying once the link returns.
func (s *Session) AddPeriodicScan(ctx context.Context, period time.Duration, mask dnp3.Class) error {
	if period <= 0 {
		return fmt.Errorf("master: %w: period must be positive", dnp3.ErrBadConfig)
	}
	if mask == 0 {
		return fmt.Errorf("master: %w: empty class mask", dnp3.ErrBadConfig)
	}

	t := newScanTask(mask, s.handler)
	t.period = period

	select {
	case s.submit <- t:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Restart asks the outstation to restart and waits for its acknowledgement.
//
// The outstation answers with how long it expects to be unavailable before it
// actually restarts, so a successful return means the request was accepted,
// not that the device is back.
func (s *Session) Restart(ctx context.Context, mode dnp3.RestartMode) error {
	return s.run(ctx, newRestartTask(mode))
}

// WriteTime sets the outstation's clock.
func (s *Session) WriteTime(ctx context.Context, t time.Time) error {
	return s.run(ctx, newWriteTimeTask(t))
}

// SyncTime sets the outstation's clock to the current time using the LAN
// procedure, which writes the time directly.
//
// It assumes the transit delay is negligible against the outstation's
// timestamp resolution — true over Ethernet, not over a slow serial link. Use
// [Session.SyncTimeWithDelay] there.
func (s *Session) SyncTime(ctx context.Context) error {
	return s.WriteTime(ctx, time.Now())
}

// SyncTimeWithDelay sets the outstation's clock using the serial procedure:
// measure the turnaround with DELAY_MEASURE, then write a time already
// corrected for it.
//
// The correction is the round trip less the outstation's own processing delay,
// halved — the one-way transit. Without it the outstation's clock lands late by
// that amount, which over a 1200 baud link is easily tens of milliseconds and
// puts every event it stamps into the past.
//
// The two requests are chained so nothing can be scheduled between them; a
// poll landing in the middle would be measured into the correction.
func (s *Session) SyncTimeWithDelay(ctx context.Context) error {
	var outstationDelay uint32
	var sentAt, gotAt time.Time

	measure := newDelayMeasureTask(&outstationDelay)
	measure.onDone = func(app.IIN) { gotAt = time.Now() }
	measure.next = func() *task {
		// Round trip minus what the outstation says it spent, halved.
		roundTrip := gotAt.Sub(sentAt)
		processing := time.Duration(outstationDelay) * time.Millisecond
		oneWay := max((roundTrip-processing)/2, 0)

		s.log.Debug("serial time sync",
			"round_trip", roundTrip, "outstation_delay", processing, "one_way", oneWay)
		return newWriteTimeTask(time.Now().Add(oneWay))
	}

	sentAt = time.Now()
	return s.run(ctx, measure)
}

// WriteDeadband sets the analog deadbands of one or more points.
//
// A deadband is how a master tells an outstation how much a point must move
// before it is worth an event. Raising it is the usual answer to an analog
// that chatters; lowering it is how a point that matters gets reported
// promptly.
func (s *Session) WriteDeadband(ctx context.Context, deadbands map[uint16]float32) error {
	if len(deadbands) == 0 {
		return fmt.Errorf("master: %w: no deadbands", dnp3.ErrBadConfig)
	}
	if len(deadbands) > 255 {
		return fmt.Errorf("master: %w: %d deadbands exceeds the 255 a one-octet count can carry",
			dnp3.ErrBadConfig, len(deadbands))
	}
	return s.run(ctx, newWriteDeadbandTask(deadbands))
}

// EnableUnsolicited turns on unsolicited reporting for a set of classes.
func (s *Session) EnableUnsolicited(ctx context.Context, mask dnp3.Class) error {
	return s.run(ctx, newUnsolicitedTask(true, mask))
}

// DisableUnsolicited turns off unsolicited reporting for a set of classes.
func (s *Session) DisableUnsolicited(ctx context.Context, mask dnp3.Class) error {
	return s.run(ctx, newUnsolicitedTask(false, mask))
}
