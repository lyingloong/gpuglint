package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/lyingloong/gpuglint/internal/collect"
	"github.com/lyingloong/gpuglint/internal/state"
)

type failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type hostState struct {
	Alias       string        `json:"alias"`
	Status      string        `json:"status"`
	LastAttempt *time.Time    `json:"last_attempt_at"`
	LastSuccess *time.Time    `json:"last_success_at"`
	FreshUntil  *time.Time    `json:"fresh_until"`
	Backend     string        `json:"backend,omitempty"`
	GPUs        []collect.GPU `json:"gpus"`
	Error       *failure      `json:"error"`
}

type snapshot struct {
	Version         int         `json:"schema_version"`
	Started         time.Time   `json:"started_at"`
	Updated         time.Time   `json:"updated_at"`
	IntervalSeconds float64     `json:"interval_seconds,omitempty"`
	Hosts           []hostState `json:"hosts"`
}

func newSnapshot(names []string, interval time.Duration) snapshot {
	now := time.Now().UTC()
	s := snapshot{Version: 1, Started: now, Updated: now, IntervalSeconds: interval.Seconds(), Hosts: make([]hostState, len(names))}
	for i, name := range names {
		s.Hosts[i] = hostState{Alias: name, Status: "pending", GPUs: []collect.GPU{}}
	}
	return s
}

type pollFunc func(context.Context, string, string) collect.Result

type update struct {
	index          int
	started, ended time.Time
	result         collect.Result
}

func (s *snapshot) apply(u update, o options) {
	h := &s.Hosts[u.index]
	h.LastAttempt = &u.started
	s.Updated = time.Now().UTC()
	if u.result.Code != "" {
		h.Status = "error"
		h.Error = &failure{Code: u.result.Code, Message: u.result.Message}
		return
	}
	h.Status, h.Error, h.Backend, h.LastSuccess = "ok", nil, u.result.Backend, &u.ended
	h.GPUs = u.result.GPUs
	if len(h.GPUs) == 0 {
		h.Status, h.GPUs = "no_gpu", []collect.GPU{}
	}
	if o.interval > 0 {
		deadline := u.ended.Add(o.interval).Add(o.timeout)
		h.FreshUntil = &deadline
	}
}

// Each host has at most one outstanding poll. The semaphore limits total SSH
// load; idle hosts release it before waiting for their next polling interval.
func startPolls(parent context.Context, names []string, o options, poll pollFunc) (<-chan update, func()) {
	ctx, cancel := context.WithCancel(parent)
	updates := make(chan update)
	limit := make(chan struct{}, o.concurrency)
	var workers sync.WaitGroup
	for i, name := range names {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case limit <- struct{}{}:
				}
				started := time.Now().UTC()
				probe, stop := context.WithTimeout(ctx, o.timeout)
				r := poll(probe, name, o.config)
				stop()
				ended := time.Now().UTC()
				<-limit
				select {
				case <-ctx.Done():
					return
				case updates <- update{i, started, ended, r}:
				}
				if o.interval == 0 {
					return
				}
				next := time.NewTimer(max(0, o.interval-time.Since(started)))
				select {
				case <-next.C:
				case <-ctx.Done():
					next.Stop()
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(updates); close(done) }()
	return updates, func() { cancel(); <-done }
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func scan(ctx context.Context, out io.Writer, o options, names []string, poll pollFunc) (int, error) {
	s := newSnapshot(names, 0)
	updates, stop := startPolls(ctx, names, o, poll)
	defer stop()
	code := 0
	for u := range updates {
		s.apply(u, o)
		if u.result.Code != "" {
			code = 2
		}
	}
	if ctx.Err() != nil {
		return 1, ctx.Err()
	}
	if o.json {
		return code, writeJSON(out, s)
	}
	return code, drawTable(out, s, o.order)
}

func publish(w *state.Writer, s snapshot) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return w.Write(append(data, '\n'))
}

func watch(ctx context.Context, out io.Writer, o options, names []string, poll pollFunc) (err error) {
	w, err := state.Open(o.path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	s := newSnapshot(names, o.interval)
	if err := publish(w, s); err != nil {
		return err
	}
	var screen *dashboard
	var ticks <-chan time.Time
	if !o.headless {
		screen = openDashboard(out)
		defer screen.close()
		if err := screen.draw(s, o.order); err != nil {
			return err
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	updates, stop := startPolls(ctx, names, o, poll)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			if err := screen.draw(s, o.order); err != nil {
				return err
			}
		case u, ok := <-updates:
			if !ok {
				return nil
			}
			s.apply(u, o)
			if err := publish(w, s); err != nil {
				return fmt.Errorf("publish snapshot: %w", err)
			}
		}
	}
}
