// +build !bootstrap

// Package metrics contains support for reporting metrics to an external server,
// currently a Prometheus pushgateway. Because plz runs as a transient process
// we can't wait around for Prometheus to call us, we've got to push to them.
package metrics

import (
	"fmt"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/shlex"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	"gopkg.in/op/go-logging.v1"

	"core"
)

var log = logging.MustGetLogger("metrics")

// This is the maximum number of errors after which plz will stop attempting to send metrics.
const maxErrors = 3

type metrics struct {
	url                                           string
	newMetrics                                    bool
	ticker                                        *time.Ticker
	cancelled                                     bool
	perTest                                       bool
	errors                                        int
	pushes                                        int
	timeout                                       time.Duration
	buildCounter, cacheCounter, testCounter       *prometheus.CounterVec
	buildHistogram, cacheHistogram, testHistogram *prometheus.HistogramVec
}

// m is the singleton metrics instance.
var m *metrics

// initOnce is used to ensure that InitFromConfig only initialises once (because Prometheus panics otherwise).
var initOnce sync.Once

// InitFromConfig sets up the initial metrics from the configuration.
func InitFromConfig(config *core.Configuration) {
	if config.Metrics.PushGatewayURL != "" {
		defer func() {
			if r := recover(); r != nil {
				log.Fatalf("%s", r)
			}
		}()

		initOnce.Do(func() {
			m = initMetrics(config.Metrics.PushGatewayURL.String(), time.Duration(config.Metrics.PushFrequency),
				time.Duration(config.Metrics.PushTimeout), config.CustomMetricLabels, config.Metrics.PerTest)
			prometheus.MustRegister(m.buildCounter)
			prometheus.MustRegister(m.cacheCounter)
			prometheus.MustRegister(m.testCounter)
			prometheus.MustRegister(m.buildHistogram)
			prometheus.MustRegister(m.cacheHistogram)
			prometheus.MustRegister(m.testHistogram)
		})
	}
}

// initMetrics initialises a new metrics instance.
// This is deliberately not exposed but is useful for testing.
func initMetrics(url string, frequency, timeout time.Duration, customLabels map[string]string, perTest bool) *metrics {
	u, err := user.Current()
	if err != nil {
		log.Warning("Can't determine current user name for metrics")
		u = &user.User{Username: "unknown"}
	}
	constLabels := prometheus.Labels{
		"user": u.Username,
		"arch": runtime.GOOS + "_" + runtime.GOARCH,
	}
	for k, v := range customLabels {
		constLabels[k] = deriveLabelValue(v)
	}

	m = &metrics{
		url:     url,
		timeout: timeout,
		ticker:  time.NewTicker(frequency),
		perTest: perTest,
	}

	// Count of builds for each target.
	m.buildCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "build_counts",
		Help:        "Count of number of times each target is built",
		ConstLabels: constLabels,
	}, []string{"success", "incremental"})

	// Count of cache hits for each target
	m.cacheCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "cache_hits",
		Help:        "Count of number of times we successfully retrieve from the cache",
		ConstLabels: constLabels,
	}, []string{"hit"})

	// Count of test runs for each target
	m.testCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "test_runs",
		Help:        "Count of number of times we run each test",
		ConstLabels: constLabels,
	}, addTest([]string{"pass"}, perTest))

	// Build durations for each target
	m.buildHistogram = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "build_durations_histogram",
		Help:        "Durations of individual build targets",
		Buckets:     prometheus.LinearBuckets(0, 0.1, 100),
		ConstLabels: constLabels,
	}, []string{})

	// Cache retrieval durations for each target
	m.cacheHistogram = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "cache_durations_histogram",
		Help:        "Durations to retrieve artifacts from the cache",
		Buckets:     prometheus.LinearBuckets(0, 0.1, 100),
		ConstLabels: constLabels,
	}, []string{})

	// Test durations for each target
	m.testHistogram = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "test_durations_histogram",
		Help:        "Durations to run tests",
		Buckets:     prometheus.LinearBuckets(0, 1, 100),
		ConstLabels: constLabels,
	}, addTest([]string{}, perTest))

	go m.keepPushing()

	return m
}

// addTest adds a per-test label to the given slice.
func addTest(s []string, perTest bool) []string {
	if perTest {
		return append(s, "test")
	}
	return s
}

// Stop shuts down the metrics and ensures the final ones are sent before returning.
func Stop() {
	if m != nil {
		m.stop()
	}
}

func (m *metrics) stop() {
	m.ticker.Stop()
	if !m.cancelled {
		m.errors = m.pushMetrics()
	}
}

// Record records metrics for the given target.
func Record(target *core.BuildTarget, duration time.Duration) {
	if m != nil {
		m.record(target, duration)
	}
}

func (m *metrics) record(target *core.BuildTarget, duration time.Duration) {
	if target.Results.NumTests > 0 {
		// Tests have run
		m.cacheCounter.WithLabelValues(b(target.Results.Cached)).Inc()
		if m.perTest {
			m.testCounter.WithLabelValues(b(target.Results.Failed == 0), target.Label.String()).Inc()
		} else {
			m.testCounter.WithLabelValues(b(target.Results.Failed == 0)).Inc()
		}
		if target.Results.Cached {
			m.cacheHistogram.WithLabelValues().Observe(duration.Seconds())
		} else if target.Results.Failed == 0 {
			if m.perTest {
				m.testHistogram.WithLabelValues(target.Label.String()).Observe(duration.Seconds())
			} else {
				m.testHistogram.WithLabelValues().Observe(duration.Seconds())
			}
		}
	} else {
		// Build has run
		state := target.State()
		m.cacheCounter.WithLabelValues(b(state == core.Cached)).Inc()
		m.buildCounter.WithLabelValues(b(state != core.Failed), b(state != core.Reused)).Inc()
		if state == core.Cached {
			m.cacheHistogram.WithLabelValues().Observe(duration.Seconds())
		} else if state != core.Failed && state >= core.Built {
			m.buildHistogram.WithLabelValues().Observe(duration.Seconds())
		}
	}
	m.newMetrics = true
}

func b(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func (m *metrics) keepPushing() {
	for range m.ticker.C {
		m.errors = m.pushMetrics()
		if m.errors >= maxErrors {
			log.Warning("Metrics don't seem to be working, giving up")
			m.cancelled = true
			return
		}
	}
}

// deadline applies a deadline to an arbitrary function and returns when either the function
// completes or the deadline expires.
func deadline(f func() error, timeout time.Duration) error {
	c := make(chan error)
	go func() {
		c <- f()
	}()
	select {
	case err := <-c:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("Metrics push timed out")
	}
}

// pushMetrics attempts to send some new metrics to the server. It returns the new number of errors.
func (m *metrics) pushMetrics() int {
	if !m.newMetrics {
		return m.errors
	}
	start := time.Now()
	m.newMetrics = false
	if err := deadline(func() error {
		return push.AddFromGatherer("please", push.HostnameGroupingKey(), m.url, prometheus.DefaultGatherer)
	}, m.timeout); err != nil {
		log.Warning("Could not push metrics to the repository: %s", err)
		m.newMetrics = true
		return m.errors + 1
	}
	m.pushes++
	log.Debug("Push #%d of metrics in %0.3fs", m.pushes, time.Since(start).Seconds())
	return 0
}

// deriveLabelValue runs a command and returns its output.
// It returns the empty string on error; we assume it's better to keep the set of labels constant on failure.
func deriveLabelValue(cmd string) string {
	parts, err := shlex.Split(cmd)
	if err != nil {
		panic(fmt.Sprintf("Invalid custom metric command [%s]: %s", cmd, err))
	}
	log.Debug("Running custom label command: %s", cmd)
	b, err := core.ExecCommand(parts[0], parts[1:]...).Output()
	log.Debug("Got output: %s", b)
	if err != nil {
		panic(fmt.Sprintf("Custom metric command [%s] failed: %s", cmd, err))
	}
	value := strings.TrimSpace(string(b))
	if strings.Contains(value, "\n") {
		panic(fmt.Sprintf("Return value of custom metric command [%s] contains spaces: %s", cmd, value))
	}
	return value
}
