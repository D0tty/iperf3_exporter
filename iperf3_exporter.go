// Copyright 2019 Edgard Castro
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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	_ "net/http/pprof"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace = "iperf3"
)

func GetCacheTimeOrDefault() time.Duration {
	strCache, ok := os.LookupEnv("CACHE_TIME")
	if ok {
		intCache, err := strconv.ParseInt(strCache, 10, 64)
		if err == nil {
			return time.Minute * time.Duration(intCache)
		}
	}
	return time.Hour * time.Duration(1)
}

var (
	listenAddress = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9579").String()
	metricsPath   = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
	timeout       = kingpin.Flag("iperf3.timeout", "iperf3 run timeout.").Default("30s").Duration()

	// Metrics about the iperf3 exporter itself.
	iperfDuration = prometheus.NewSummary(prometheus.SummaryOpts{Name: prometheus.BuildFQName(namespace, "exporter", "duration_seconds"), Help: "Duration of collections by the iperf3 exporter."})
	iperfErrors   = prometheus.NewCounter(prometheus.CounterOpts{Name: prometheus.BuildFQName(namespace, "exporter", "errors_total"), Help: "Errors raised by the iperf3 exporter."})

	cacheMap  = make(map[string]*CacheExporter)
	cacheTime = GetCacheTimeOrDefault()
)

type CacheExporter struct {
	lastExport            time.Time
	cachedThread          int
	cachedSentSeconds     float64
	cachedSentBytes       float64
	cachedReceivedSeconds float64
	cachedReceivedBytes   float64
}

func NewCacheExporter(exportTime time.Time, thread int, sentSeconds float64, sentBytes float64, receivedSeconds float64, receivedBytes float64) *CacheExporter {
	return &CacheExporter{
		lastExport:            exportTime,
		cachedThread:          thread,
		cachedSentSeconds:     sentSeconds,
		cachedSentBytes:       sentBytes,
		cachedReceivedSeconds: receivedSeconds,
		cachedReceivedBytes:   receivedBytes,
	}
}

func NewExpiredCacheExporter() *CacheExporter {
	return &CacheExporter{
		lastExport:            time.Now().AddDate(-1, 0, 0), // create expired cache on purpose
		cachedThread:          0,
		cachedSentSeconds:     0,
		cachedSentBytes:       0,
		cachedReceivedSeconds: 0,
		cachedReceivedBytes:   0,
	}
}

// iperfResult collects the partial result from the iperf3 run
type iperfResult struct {
	End struct {
		SumSent struct {
			Seconds float64 `json:"seconds"`
			Bytes   float64 `json:"bytes"`
		} `json:"sum_sent"`
		SumReceived struct {
			Seconds float64 `json:"seconds"`
			Bytes   float64 `json:"bytes"`
		} `json:"sum_received"`
	} `json:"end"`
}

// Exporter collects iperf3 stats from the given address and exports them using
// the prometheus metrics package.
type Exporter struct {
	target  string
	port    int
	thread  int
	period  time.Duration
	timeout time.Duration
	mutex   sync.RWMutex

	nbThread        *prometheus.Desc
	success         *prometheus.Desc
	sentSeconds     *prometheus.Desc
	sentBytes       *prometheus.Desc
	receivedSeconds *prometheus.Desc
	receivedBytes   *prometheus.Desc
}

// NewExporter returns an initialized Exporter.
func NewExporter(target string, port int, thread int, period time.Duration, timeout time.Duration) *Exporter {
	return &Exporter{
		target:          target,
		port:            port,
		thread:          thread,
		period:          period,
		timeout:         timeout,
		nbThread:        prometheus.NewDesc(prometheus.BuildFQName(namespace, "", "nb_thread"), "Total number of thread used by the client.", nil, nil),
		success:         prometheus.NewDesc(prometheus.BuildFQName(namespace, "", "success"), "Was the last iperf3 probe successful.", nil, nil),
		sentSeconds:     prometheus.NewDesc(prometheus.BuildFQName(namespace, "", "sent_seconds"), "Total seconds spent sending packets.", nil, nil),
		sentBytes:       prometheus.NewDesc(prometheus.BuildFQName(namespace, "", "sent_bytes"), "Total sent bytes.", nil, nil),
		receivedSeconds: prometheus.NewDesc(prometheus.BuildFQName(namespace, "", "received_seconds"), "Total seconds spent receiving packets.", nil, nil),
		receivedBytes:   prometheus.NewDesc(prometheus.BuildFQName(namespace, "", "received_bytes"), "Total received bytes.", nil, nil),
	}
}

// Describe describes all the metrics exported by the iperf3 exporter. It
// implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.nbThread
	ch <- e.success
	ch <- e.sentSeconds
	ch <- e.sentBytes
	ch <- e.receivedSeconds
	ch <- e.receivedBytes
}

// Collect probes the configured iperf3 server and delivers them as Prometheus
// metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock() // To protect metrics from concurrent collects.
	defer e.mutex.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()

	currentCacheExport, ok := cacheMap[e.target]

	if !ok {
		cacheMap[e.target] = NewExpiredCacheExporter()
		currentCacheExport, ok = cacheMap[e.target]
	}

	if time.Now().Sub(currentCacheExport.lastExport) >= cacheTime {

		var iperfArgs []string
		iperfArgs = append(iperfArgs, "-J", "-t", strconv.FormatFloat(e.period.Seconds(), 'f', 0, 64), "-c", e.target, "-p", strconv.Itoa(e.port))

		iperfArgs = append(iperfArgs, "-P", strconv.Itoa(e.thread))

		out, err := exec.CommandContext(ctx, iperfCmd, iperfArgs...).Output()

		if err != nil {
			ch <- prometheus.MustNewConstMetric(e.success, prometheus.GaugeValue, 0)
			iperfErrors.Inc()
			log.Errorf("Failed to run iperf3: %s", err)
			return
		}

		stats := iperfResult{}
		if err := json.Unmarshal(out, &stats); err != nil {
			ch <- prometheus.MustNewConstMetric(e.success, prometheus.GaugeValue, 0)
			iperfErrors.Inc()
			log.Errorf("Failed to parse iperf3 result: %s", err)
			return
		}

		currentCacheExport.cachedThread = e.thread
		currentCacheExport.cachedSentSeconds = stats.End.SumSent.Seconds
		currentCacheExport.cachedSentBytes = stats.End.SumSent.Bytes
		currentCacheExport.cachedReceivedSeconds = stats.End.SumReceived.Seconds
		currentCacheExport.cachedReceivedBytes = stats.End.SumReceived.Bytes
		currentCacheExport.lastExport = time.Now()

	}

	ch <- prometheus.MustNewConstMetric(e.nbThread, prometheus.GaugeValue, float64(currentCacheExport.cachedThread))
	ch <- prometheus.MustNewConstMetric(e.success, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(e.sentSeconds, prometheus.GaugeValue, currentCacheExport.cachedSentSeconds)
	ch <- prometheus.MustNewConstMetric(e.sentBytes, prometheus.GaugeValue, currentCacheExport.cachedSentBytes)
	ch <- prometheus.MustNewConstMetric(e.receivedSeconds, prometheus.GaugeValue, currentCacheExport.cachedReceivedSeconds)
	ch <- prometheus.MustNewConstMetric(e.receivedBytes, prometheus.GaugeValue, currentCacheExport.cachedReceivedBytes)
}

func handler(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "'target' parameter must be specified", http.StatusBadRequest)
		iperfErrors.Inc()
		return
	}

	var targetPort int
	port := r.URL.Query().Get("port")
	if port != "" {
		var err error
		targetPort, err = strconv.Atoi(port)
		if err != nil {
			http.Error(w, fmt.Sprintf("'port' parameter must be an integer: %s", err), http.StatusBadRequest)
			iperfErrors.Inc()
			return
		}
	}
	if targetPort == 0 {
		targetPort = 5201
	}

	var targetThread int
	thread := r.URL.Query().Get("thread")
	if thread != "" {
		var err error
		targetThread, err = strconv.Atoi(thread)
		if err != nil {
			http.Error(w, fmt.Sprintf("'thread' parameter must be an integer: %s", err), http.StatusBadRequest)
			iperfErrors.Inc()
			return
		}
	}
	if targetThread <= 0 {
		targetThread = 1
	}

	var runPeriod time.Duration
	period := r.URL.Query().Get("period")
	if period != "" {
		var err error
		runPeriod, err = time.ParseDuration(period)
		if err != nil {
			http.Error(w, fmt.Sprintf("'period' parameter must be a duration: %s", err), http.StatusBadRequest)
			iperfErrors.Inc()
			return
		}
	}
	if runPeriod.Seconds() == 0 {
		runPeriod = time.Second * 5
	}

	// If a timeout is configured via the Prometheus header, add it to the request.
	var timeoutSeconds float64
	if v := r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds"); v != "" {
		var err error
		timeoutSeconds, err = strconv.ParseFloat(v, 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to parse timeout from Prometheus header: %s", err), http.StatusInternalServerError)
			iperfErrors.Inc()
			return
		}
	}
	if timeoutSeconds == 0 {
		if timeout.Seconds() > 0 {
			timeoutSeconds = timeout.Seconds()
		} else {
			timeoutSeconds = 30
		}
	}

	if timeoutSeconds > 30 {
		timeoutSeconds = 30
	}

	runTimeout := time.Duration(timeoutSeconds * float64(time.Second))

	start := time.Now()
	registry := prometheus.NewRegistry()
	exporter := NewExporter(target, targetPort, targetThread, runPeriod, runTimeout)
	registry.MustRegister(exporter)

	// Delegate http serving to Prometheus client library, which will call collector.Collect.
	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)

	duration := time.Since(start).Seconds()
	iperfDuration.Observe(duration)
}

func main() {
	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print("iperf3_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	log.Info("Starting iperf3 exporter", version.Info())
	log.Info("Build context", version.BuildContext())

	prometheus.MustRegister(version.NewCollector("iperf3_exporter"))
	prometheus.MustRegister(iperfDuration)
	prometheus.MustRegister(iperfErrors)

	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/probe", handler)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, err := w.Write([]byte(`<html>
    <head><title>iPerf3 Exporter</title></head>
    <body>
    <h1>iPerf3 Exporter</h1>
    <p><a href="/probe?target=prometheus.io">Probe prometheus.io</a></p>
    <p><a href='` + *metricsPath + `'>Metrics</a></p>
    </html>`))
		if err != nil {
			log.Warnf("Failed to write to HTTP client: %s", err)
		}
	})

	srv := &http.Server{
		Addr:         *listenAddress,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	log.Infof("Caching enabled, duration: %s", cacheTime)
	log.Infof("Listening on %s", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}
