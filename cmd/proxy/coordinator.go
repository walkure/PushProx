// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/google/uuid"
	"github.com/prometheus-community/pushprox/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	registrationTimeout = kingpin.Flag("registration.timeout", "After how long a registration expires.").Default("5m").Duration()
)

// Coordinator metrics.
var (
	knownClients = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "clients",
			Help:      "Number of known pushprox clients.",
		},
	)
)

// Coordinator for scrape requests and responses
type Coordinator struct {
	mu sync.Mutex

	// Clients waiting for a scrape.
	waiting map[string]chan *http.Request
	// Responses from clients.
	responses map[string]chan *http.Response
	// Clients we know about and when they last contacted us.
	known map[string]time.Time

	logger log.Logger
}

// NewCoordinator initiates the coordinator and starts the client cleanup routine
func NewCoordinator(logger log.Logger) (*Coordinator, error) {
	c := &Coordinator{
		waiting:   map[string]chan *http.Request{},
		responses: map[string]chan *http.Response{},
		known:     map[string]time.Time{},
		logger:    logger,
	}

	go c.gc()
	return c, nil
}

// Generate a unique ID
func (c *Coordinator) genID() (string, error) {
	id, err := uuid.NewRandom()
	return id.String(), err
}

func (c *Coordinator) getRequestChannel(fqdn string) chan *http.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.waiting[fqdn]
	if !ok {
		ch = make(chan *http.Request)
		c.waiting[fqdn] = ch
	}
	return ch
}

func (c *Coordinator) getResponseChannel(id string) chan *http.Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.responses[id]
	if !ok {
		ch = make(chan *http.Response)
		c.responses[id] = ch
	}
	return ch
}

// Remove a response channel. Idempotent.
func (c *Coordinator) removeResponseChannel(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.responses, id)
}

// DoScrape requests a scrape.
func (c *Coordinator) DoScrape(ctx context.Context, r *http.Request) (*http.Response, error) {
	id, err := c.genID()
	if err != nil {
		return nil, err
	}
	level.Info(c.logger).Log("msg", "DoScrape", "scrape_id", id, "url", r.URL.String())
	r.Header.Add("Id", id)
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("Request Timeout reached for %q(id:%s): %w", r.URL.String(), id, ctx.Err())
	case c.getRequestChannel(r.URL.Hostname()) <- r:
	}

	respCh := c.getResponseChannel(id)
	defer c.removeResponseChannel(id)

	select {
	case <-r.Context().Done():
		return nil, fmt.Errorf("Scrape timeout reached for %q(id:%s): %w", r.URL.String(), id, r.Context().Err())
	case <-ctx.Done():
		return nil, fmt.Errorf("Response timeout reached for %q(id:%s): %w", r.URL.String(), id, ctx.Err())
	case resp := <-respCh:
		return resp, nil
	}
}

// WaitForScrapeInstruction registers a client waiting for a scrape result
func (c *Coordinator) WaitForScrapeInstruction(poll *http.Request) (*http.Request, error) {
	fqdnBytes, err := io.ReadAll(poll.Body)
	if err != nil {
		return nil, fmt.Errorf("failure to receive poll request:%w", err)
	}
	fqdn := strings.TrimSpace(string(fqdnBytes))
	level.Info(c.logger).Log("msg", "WaitForScrapeInstruction", "fqdn", fqdn)

	c.addKnownClient(fqdn)
	// TODO: What if the client times out?
	ch := c.getRequestChannel(fqdn)

	// exhaust existing poll request (eg. timeouted queues)
	select {
	case ch <- nil:
		//
	default:
		break
	}

	ctx := poll.Context()
	if timeout := poll.Header.Get("X-PushProx-Polling-Timeout"); timeout != "" {
		tmDuration, err := time.ParseDuration(timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid polling timeout value:%w", err)
		}
		newCtx, cancel := context.WithTimeout(ctx, tmDuration)
		ctx = newCtx
		defer cancel()
	}

	for {
		select {
		case request := <-ch:
			if request == nil {
				return nil, fmt.Errorf("request is expired")
			}

			select {
			case <-request.Context().Done():
				level.Warn(c.logger).Log("msg", "request is timeout", "err", request.Context().Err())
				// Request has timed out, get another one.
			case <-ctx.Done():
				return nil, fmt.Errorf("polling timeout: %w", ctx.Err())
			default:
				return request, nil
			}

		case <-ctx.Done():
			return nil, fmt.Errorf("request polling timeout: %w", ctx.Err())
		}
	}
}

// ScrapeResult send by client
func (c *Coordinator) ScrapeResult(r *http.Response) error {
	id := r.Header.Get("Id")
	level.Info(c.logger).Log("msg", "ScrapeResult", "scrape_id", id)
	ctx, cancel := context.WithTimeout(context.Background(), util.GetScrapeTimeout(maxScrapeTimeout, defaultScrapeTimeout, r.Header))
	defer cancel()
	// Don't expose internal headers.
	r.Header.Del("Id")
	r.Header.Del("X-Prometheus-Scrape-Timeout-Seconds")
	select {
	case c.getResponseChannel(id) <- r:
		return nil
	case <-ctx.Done():
		c.removeResponseChannel(id)
		return ctx.Err()
	}
}

func (c *Coordinator) addKnownClient(fqdn string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.known[fqdn] = time.Now()
	knownClients.Set(float64(len(c.known)))
}

// KnownClients returns a list of alive clients
func (c *Coordinator) KnownClients() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	limit := time.Now().Add(-*registrationTimeout)
	known := make([]string, 0, len(c.known))
	for k, t := range c.known {
		if limit.Before(t) {
			known = append(known, k)
		}
	}
	return known
}

// Garbagee collect old clients.
func (c *Coordinator) gc() {
	for range time.Tick(1 * time.Minute) {
		func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			limit := time.Now().Add(-*registrationTimeout)
			deleted := 0
			for k, ts := range c.known {
				if ts.Before(limit) {
					delete(c.known, k)
					deleted++
				}
			}
			level.Info(c.logger).Log("msg", "GC of clients completed", "deleted", deleted, "remaining", len(c.known))
			knownClients.Set(float64(len(c.known)))
		}()
	}
}
