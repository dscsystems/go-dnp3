package secevent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
)

// ChannelSink sends events to a Go channel without blocking, counting the ones
// it had to drop because the channel was full.
type ChannelSink struct {
	C       chan<- Event
	dropped atomic.Uint64
}

// NewChannelSink returns a sink sending to c.
func NewChannelSink(c chan<- Event) *ChannelSink { return &ChannelSink{C: c} }

// HandleEvent sends e, or drops it if the channel is full.
func (s *ChannelSink) HandleEvent(e Event) {
	select {
	case s.C <- e:
	default:
		s.dropped.Add(1)
	}
}

// Dropped returns how many events the full channel cost.
func (s *ChannelSink) Dropped() uint64 { return s.dropped.Load() }

// SlogSink logs events to l, mapping Critical and Alert to Error, Warning to
// Warn, and Notice and Info to Info.
func SlogSink(l *slog.Logger) Sink {
	return SinkFunc(func(e Event) {
		level := slog.LevelInfo
		switch {
		case e.Severity >= Alert:
			level = slog.LevelError
		case e.Severity == Warning:
			level = slog.LevelWarn
		}
		attrs := []slog.Attr{
			slog.String("kind", string(e.Kind)),
			slog.String("severity", e.Severity.String()),
			slog.String("category", e.Category.String()),
		}
		if e.Action != Observed {
			attrs = append(attrs, slog.String("action", e.Action.String()))
		}
		if e.Role != "" {
			attrs = append(attrs, slog.String("role", e.Role))
		}
		if e.ConnID != 0 {
			attrs = append(attrs, slog.Uint64("conn", e.ConnID))
		}
		if p := e.Peer.String(); p != "" {
			attrs = append(attrs, slog.String("peer", p))
		}
		if e.Fields&HasLinkAddrs != 0 {
			attrs = append(attrs, slog.Int("src", int(e.LinkSrc)), slog.Int("dest", int(e.LinkDest)))
		}
		if e.Fields&HasAppFunc != 0 {
			attrs = append(attrs, slog.String("func", FuncName(e.AppFunc)))
		}
		if e.Fields&HasIndex != 0 {
			attrs = append(attrs, slog.Uint64("index", uint64(e.Index)))
		}
		if e.Count > 1 {
			attrs = append(attrs, slog.Int("count", e.Count))
		}
		if e.Detail != "" {
			attrs = append(attrs, slog.String("detail", e.Detail))
		}
		if e.Err != nil {
			attrs = append(attrs, slog.String("err", e.Err.Error()))
		}
		l.LogAttrs(context.Background(), level, "cyber "+string(e.Kind), attrs...)
	})
}

// JSONLinesSink writes each event as one line of JSON. Write errors are
// counted by the returned sink rather than reported, since nothing upstream
// could act on them.
func JSONLinesSink(w io.Writer) *JSONSink { return &JSONSink{w: w} }

// JSONSink is the sink [JSONLinesSink] returns.
type JSONSink struct {
	mu     sync.Mutex
	w      io.Writer
	errors atomic.Uint64
}

// HandleEvent writes e as a line of JSON.
func (s *JSONSink) HandleEvent(e Event) {
	b, err := json.Marshal(e)
	if err != nil {
		s.errors.Add(1)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(append(b, '\n')); err != nil {
		s.errors.Add(1)
	}
}

// Errors returns how many events could not be written.
func (s *JSONSink) Errors() uint64 { return s.errors.Load() }

// MultiSink delivers every event to each sink in turn.
func MultiSink(sinks ...Sink) Sink {
	sinks = slices.Clone(sinks)
	return SinkFunc(func(e Event) {
		for _, s := range sinks {
			s.HandleEvent(e)
		}
	})
}

// FilterSink passes on events of at least min severity and, when cats is not
// empty, of one of those categories.
func FilterSink(min Severity, cats []Category, s Sink) Sink {
	cats = slices.Clone(cats)
	return SinkFunc(func(e Event) {
		if e.Severity < min || (len(cats) > 0 && !slices.Contains(cats, e.Category)) {
			return
		}
		s.HandleEvent(e)
	})
}
