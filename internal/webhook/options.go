package webhook

import (
	"log/slog"
	"net/http"
	"time"
)

// Option configures a Dispatcher.
type Option func(*options)

type options struct {
	workers     int
	maxAttempts int
	sweepPeriod time.Duration
	client      *http.Client
	logger      *slog.Logger
}

// WithWorkers sets the number of concurrent delivery workers (default 4).
func WithWorkers(n int) Option { return func(o *options) { o.workers = n } }

// WithMaxAttempts sets the maximum delivery attempts before marking a job dead (default 5).
func WithMaxAttempts(n int) Option { return func(o *options) { o.maxAttempts = n } }

// WithHTTPClient overrides the HTTP client used for webhook POSTs.
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.client = c } }

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithSweepPeriod overrides the interval at which the sweeper re-enqueues
// pending jobs that were missed (default 30s).
func WithSweepPeriod(d time.Duration) Option { return func(o *options) { o.sweepPeriod = d } }
