package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
)

// session is one outstation being polled.
type session struct {
	site Site
	sess *master.Session
	ch   channel.Channel
	h    *master.ChannelHandler
}

// buildChannel selects the transport for a site.
func buildChannel(s Site) (channel.Channel, error) {
	switch {
	case s.Serial != "":
		return channel.SerialChannel(channel.SerialConfig{
			Device: s.Serial, Baud: s.Baud,
		}, channel.DefaultRetry), nil

	case s.TLS != nil:
		return channel.TLSClient(s.Host, channel.TLSConfig{
			CertFile:   s.TLS.Cert,
			KeyFile:    s.TLS.Key,
			CAFile:     s.TLS.CA,
			ServerName: s.TLS.ServerName,
		}, channel.DefaultRetry)

	default:
		return channel.TCPClient(s.Host, channel.DefaultRetry), nil
	}
}

// newSession builds one site's session.
func newSession(s Site, log *slog.Logger) (*session, error) {
	ch, err := buildChannel(s)
	if err != nil {
		return nil, fmt.Errorf("site %q: %w", s.Name, err)
	}

	// A generous buffer: an event storm from one site must not stall its own
	// session while the recorder catches up.
	h := master.NewChannelHandler(4096)

	sess := master.New(master.Config{
		LocalAddr:             s.Local,
		RemoteAddr:            s.Address,
		ResponseTimeout:       s.Timeout,
		KeepAlive:             s.KeepAlive,
		IntegrityOnStartup:    true,
		DisableUnsolOnStartup: true,
		UnsolClassMask:        s.unsolMask(),
		UseLinkConfirms:       s.LinkConfirms != nil && *s.LinkConfirms,
		LinkRetries:           3,
		Log:                   log.With("site", s.Name),
	}, h)

	return &session{site: s, sess: sess, ch: ch, h: h}, nil
}

// run polls every configured site until the context ends.
func run(ctx context.Context, cfg *Config, rec *Recorder, snap *Snapshot,
	log *slog.Logger, listen string) error {

	var sessions []*session
	for _, raw := range cfg.Sites {
		s, err := newSession(cfg.resolved(raw), log)
		if err != nil {
			return err
		}
		sessions = append(sessions, s)
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	defer func() {
		for _, s := range sessions {
			_ = s.ch.Close()
		}
	}()

	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.sess.Run(ctx, s.ch); err != nil && ctx.Err() == nil {
				log.Error("session ended", "site", s.site.Name, "err", err)
			}
		}()

		// One consumer per session, so a slow recorder delays only its own
		// site rather than every session sharing one drain loop.
		wg.Add(1)
		go func() {
			defer wg.Done()
			consume(ctx, s, rec, snap, log)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			schedule(ctx, s, log)
		}()
	}

	// A status poller, so the API and console reflect sessions that have
	// nothing to report as well as those that do.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, s := range sessions {
					st := s.sess.Stats()
					snap.SetStatus(SiteStatus{
						Name:           s.site.Name,
						Target:         s.site.describe(),
						Connected:      s.sess.Connected(),
						Indications:    s.sess.LastIIN().String(),
						TasksRun:       st.TasksRun,
						TasksSucceeded: st.TasksSucceeded,
						TasksFailed:    st.TasksFailed,
						Timeouts:       st.ResponseTimeout,
						Unsolicited:    st.Unsolicited,
						Restarts:       st.RestartsSeen,
						Connections:    st.Connections,
					})
				}
			}
		}
	}()

	if listen != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveAPI(ctx, listen, snap, log)
		}()
	}

	<-ctx.Done()
	return nil
}

// consume drains one session's measurements into the recorder and snapshot.
func consume(ctx context.Context, s *session, rec *Recorder, snap *Snapshot, log *slog.Logger) {
	var dropped uint64

	for {
		select {
		case <-ctx.Done():
			return

		case u := <-s.h.Updates():
			snap.Apply(s.site.Name, u)
			if err := rec.Record(s.site.Name, u); err != nil {
				log.Error("recording failed", "site", s.site.Name, "err", err)
			}

			// The handler drops updates when its buffer fills rather than
			// stalling the protocol. That is the right trade, but it is not
			// free, and an operator reading a recording with holes in it
			// deserves to know they are there.
			if d := s.h.Dropped(); d > dropped {
				log.Warn("updates dropped; the recorder is not keeping up",
					"site", s.site.Name, "dropped", d-dropped, "total", d)
				dropped = d
			}
		}
	}
}

// schedule sets up a site's recurring work.
func schedule(ctx context.Context, s *session, log *slog.Logger) {
	// Wait for the startup sequence rather than racing it into the queue.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}

	if s.site.SyncClock != nil && *s.site.SyncClock {
		syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.sess.SyncTime(syncCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Warn("clock sync failed", "site", s.site.Name, "err", err)
		}
	}

	if s.site.Poll > 0 {
		if err := s.sess.AddPeriodicScan(ctx, s.site.Poll, dnp3.Class123); err != nil && ctx.Err() == nil {
			log.Error("scheduling the event poll failed", "site", s.site.Name, "err", err)
		}
	}
	if s.site.Integrity > 0 {
		if err := s.sess.AddPeriodicScan(ctx, s.site.Integrity, dnp3.ClassAll); err != nil && ctx.Err() == nil {
			log.Error("scheduling the integrity poll failed", "site", s.site.Name, "err", err)
		}
	}
}

// serveAPI exposes the live snapshot over HTTP.
func serveAPI(ctx context.Context, addr string, snap *Snapshot, log *slog.Logger) {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// If the client has gone, the health check answered itself.
		_, _ = fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, snap.Status())
	})

	mux.HandleFunc("GET /points", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, snap.Points(r.URL.Query().Get("site")))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("HTTP API listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("HTTP API failed", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// discardLogger writes nothing, for tests and for the operate subcommand's
// quiet paths.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
