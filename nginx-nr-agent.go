package main

// Go replacement for the nginx New Relic plugin. No Python runtime and various
// extra modules required. Only reports on a single instance of Nginx, and takes
// configuration from environment variables.
//
// The internal design is to use the go-metrics library which is a Go port of
// the Coda Hale/Dropwizard metric package. This simplifies all the recotrd
// keeping a lot by letting the robust stats library handle it. We just read
// the values on a timed basis and report them up to New Relic.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/kelseyhightower/envconfig"
	"github.com/rcrowley/go-metrics"
	"gopkg.in/relistan/rubberneck.v1"
)

const (
	AgentGuid          = "com.nginx.newrelic-agent"
	AgentVersion       = "2.0.0"
	PollInterval       = 60 * time.Second // How often we're polling. New Relic expects 1 minute
)

var (
	ParserRegexp = regexp.MustCompile(
		"^Active connections: (?P<connections>\\d+)\\s+[\\w ]+\n" +
			"\\s+(?P<accepts>\\d+)" +
			"\\s+(?P<handled>\\d+)" +
			"\\s+(?P<requests>\\d+)" +
			"\\s+Reading:\\s+(?P<reading>\\d+)" +
			"\\s+Writing:\\s+(?P<writing>\\d+)" +
			"\\s+Waiting:\\s+(?P<waiting>\\d+)",
	)

	accepted = metrics.NewCounter()
	dropped  = metrics.NewCounter()
	total    = metrics.NewCounter()
	active   = metrics.NewGauge()
	idle     = metrics.NewGauge()
	current  = metrics.NewGauge()

	config Config
)

type Config struct {
	NewRelicAppName    string `split_words:"true"`
	NewRelicApiUrl     string `split_words:"true" default:"https://platform-api.newrelic.com/platform/v1/metrics"`
	NewRelicLicenseKey string `split_words:"true"`
	StatsUrl           string `split_words:"true"`
}

type MetricReading struct {
	Connections int64
	Accepts     int64
	Handled     int64
	Requests    int64
	Reading     int64
	Writing     int64
	Waiting     int64
}

// The data we'll report to New Relic
type NrMetric struct {
	Accepted int64 `newrelic:"Component/Connections/Accepted[Connections/sec]"`
	Dropped  int64 `newrelic:"Component/Connections/Dropped[Connections/sec]"`
	Total    int64 `newrelic:"Component/Requests/Total[Connections]"`
	Active   int64 `newrelic:"Component/Connections/Active[Connections]"`
	Idle     int64 `newrelic:"Component/Connections/Idle[Connections]"`
	Current  int64 `newrelic:"Component/Requests/Current[Requests]"`
}

type NrUpload struct {
	Agent      map[string]string `json:"agent"`
	Components []*NrComponent    `json:"components"`
}

type NrComponent struct {
	Guid     string           `json:"guid"`
	Duration int              `json:"duration"`
	Name     string           `json:"name"`
	Metrics  map[string]int64 `json:"metrics"`
}

func NewNrComponent(metrics map[string]int64) *NrComponent {
	return &NrComponent{
		Guid:     AgentGuid,
		Duration: (int)(PollInterval / time.Second),
		Name:     config.NewRelicAppName,
		Metrics:  metrics,
	}
}

func NewNrUpload(components []*NrComponent) *NrUpload {
	hostname, _ := os.Hostname()

	return &NrUpload{
		Agent: map[string]string{
			"version": AgentVersion,
			"host":    hostname,
			"pid":     strconv.Itoa(os.Getpid()),
		},
		Components: components,
	}
}

// Connect up to nginx and fetch the stub status output
func GetStats(url string) (*MetricReading, error) {
	client := &http.Client{
		Timeout: 3 * time.Second,
	}
	response, err := client.Get(url)
	if err != nil {
		return nil, err
	}

	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	match := ParserRegexp.FindStringSubmatch(string(body))
	result := make(map[string]int64)
	for i, name := range ParserRegexp.SubexpNames() {
		if i != 0 {
			val, _ := strconv.Atoi(match[i])
			result[name] = int64(val)
		}
	}

	metric := MetricReading{
		Connections: result["connections"],
		Accepts:     result["accepts"],
		Handled:     result["handled"],
		Requests:    result["requests"],
		Reading:     result["reading"],
		Writing:     result["writing"],
		Waiting:     result["waiting"],
	}

	return &metric, nil
}

// Transform the reading from Nginx into the metric values, and update
// the go-metrics values.
func processOne(metric *MetricReading) {
	accepted.Inc(metric.Accepts)
	dropped.Inc(metric.Accepts - metric.Handled)
	total.Inc(metric.Connections)
	active.Update(metric.Connections)
	idle.Update(metric.Waiting)
	current.Update(metric.Reading + metric.Writing)
}

// Format an NrMetric and put it into the upload channel
func notifyNewRelic(nrChan chan *NrMetric) {
	batch := NrMetric{
		Accepted: accepted.Count(),
		Dropped:  dropped.Count(),
		Total:    total.Count(),
		Active:   active.Value(),
		Idle:     idle.Value(),
		Current:  current.Value(),
	}

	nrChan <- &batch
}

// Runs in the background, uploading things as they arrive in the channel
func processUploads(nrChan chan *NrMetric) {
	// Uses reflection to read the struct tags... slow, but not high throughput
	for batch := range nrChan {
		st := reflect.TypeOf(*batch)
		item := reflect.ValueOf(*batch)
		metrics := make(map[string]int64, st.NumField())

		for i := 0; i < st.NumField(); i++ {
			metrics[st.Field(i).Tag.Get("newrelic")] = item.Field(i).Int()
		}

		upload := NewNrUpload([]*NrComponent{NewNrComponent(metrics)})

		err := uploadOne(upload)
		if err != nil {
			log.Errorf("Failed to upload to New Relic: %s", err)
		}
	}
}

// Handle uploading one metric batch
func uploadOne(upload *NrUpload) error {
	log.Debugf("Uploading to New Relic")
	client := &http.Client{
		Timeout: 3 * time.Second,
	}

	bodyJson, err := json.Marshal(upload)
	if err != nil {
		return fmt.Errorf("Unable to encode upload: %s", err)
	}

	log.Debug(string(bodyJson))

	uploadBody := bytes.NewBuffer(bodyJson)

	req, err := http.NewRequest("POST", config.NewRelicApiUrl, uploadBody)
	if err != nil {
		return err
	}

	// Send the required headers so the New Relic API will take our data
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("User-Agent", "newrelic-nginx-agent/"+AgentVersion)
	req.Header.Add("X-License-Key", config.NewRelicLicenseKey)

	response, err := client.Do(req)
	if err != nil {
		return err
	}

	uploadBody.Reset()

	defer response.Body.Close()

	if response.StatusCode > 299 || response.StatusCode < 200 {
		body, _ := ioutil.ReadAll(response.Body)

		return fmt.Errorf("Got invalid response from New Relic (%d): %s",
			response.StatusCode, string(body))
	}

	if err != nil {
		return err
	}

	log.Debugf("Successful upload to New Relic")

	return nil
}

// Immediately, and on a timed loop, update the metrics.
func processStats(quit chan struct{}, nrChan chan *NrMetric) {
	metric, err := GetStats(config.StatsUrl)
	if err != nil {
		log.Errorf("Unable to fetch stats from nginx: %s", err)
		return
	}

	processOne(metric)
	notifyNewRelic(nrChan)

	for {
		select {
		case <-time.After(5 * time.Second):
			metric, _ := GetStats(config.StatsUrl)
			processOne(metric)
			notifyNewRelic(nrChan)
		case <-quit:
			return
		}
	}
}

// Tell the go-metrics package about the metrics we're going to be
// using.
func RegisterMetrics() {
	metrics.Register("accepted", accepted)
	metrics.Register("dropped", dropped)
	metrics.Register("total", total)
	metrics.Register("active", active)
	metrics.Register("idle", idle)
	metrics.Register("current", current)
}

func main() {
	log.SetLevel(log.InfoLevel)

	envconfig.Process("agent", &config)
	rubberneck.Print(config)

	RegisterMetrics()

	nrChan := make(chan *NrMetric)
	quitChan := make(chan struct{})

	go processStats(quitChan, nrChan)
	go processUploads(nrChan)

	select {}
}
