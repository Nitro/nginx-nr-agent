package main

// Go replacement for the nginx New Relic plugin. No Python runtime required.
// Only reports on a single instance of Nginx, and takes configuration from
// environment variables.

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
	"gopkg.in/relistan/rubberneck.v1"
)

const (
	AgentGuid        = "com.nginx.newrelic-agent"
	AgentVersion     = "2.0.1"
	PollSeconds      = 60
	PollInterval     = PollSeconds * time.Second // How often we're polling. New Relic expects 1 minute
	ErrorBackoffTime = 10 * time.Second // How long to back off on errored stats fetch
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

	accepted    int64
	sumAccepted int64
	dropped     int64
	total       int64
	active      int64
	idle        int64
	current     int64

	config Config
)

type Config struct {
	NewRelicAppName    string `split_words:"true"`
	NewRelicApiUrl     string `split_words:"true" default:"https://platform-api.newrelic.com/platform/v1/metrics"`
	NewRelicLicenseKey string `split_words:"true"`
	StatsUrl           string `split_words:"true" default:"http://localhost:8000/status"`
	Debug              bool   `envconfig:"DEBUG" default:"false"`
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
	SummaryIdle   int64   `newrelic:"Component/ConnSummary/Idle[Connections]"`
	SummaryActive int64   `newrelic:"Component/ConnSummary/Active[Connections]"`
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
		Timeout: 7 * time.Second,
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
func processOne(metric *MetricReading) {
	// We don't want to report giant spikes on the graph on startup
	if sumAccepted != 0 {
		// Accepted is a counter... we need to subtract the total each time
		accepted = (metric.Accepts - sumAccepted) / PollSeconds // report rps not rpm
	}
	sumAccepted = metric.Accepts

	dropped = metric.Accepts - metric.Handled - dropped
	active = metric.Connections
	idle = metric.Waiting
	total = active + idle
	current = metric.Reading + metric.Writing

	log.Debugf(`
		Accepted: %d
		Dropped:  %d
		Total:    %d
		Active:   %d
		Idle:     %d
		Current:  %d
	`, accepted, dropped, total, active, idle, current,
	)
}

// Format an NrMetric and put it into the upload channel
func notifyNewRelic(nrChan chan *NrMetric) {
	batch := NrMetric{
		Accepted: accepted,
		Dropped:  dropped,
		Total:    total,
		Active:   active,
		Idle:     idle,
		Current:  current,
		SummaryIdle:   idle,
		SummaryActive: active,
	}

	select {
	case nrChan <- &batch:
		// great!
	case <-time.After(1 * time.Second):
		log.Warn("Nothing is consuming New Relic reporting events. Giving up reporting")
	}
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
		Timeout: 15 * time.Second,
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
	for {
		select {
		case <-time.After(PollInterval):
			log.Debug("Connecting to Nginx to fetch stats")
			metric, err := GetStats(config.StatsUrl)
			if err != nil {
				log.Errorf("Unable to fetch stats from nginx: %s", err)
				time.Sleep(ErrorBackoffTime)
				continue
			}
			processOne(metric)
			notifyNewRelic(nrChan)
		case <-quit:
			log.Warn("Received quit signal, shutting down")
			return
		}
	}
}

func main() {
	envconfig.Process("agent", &config)
	rubberneck.Print(config)

	if config.Debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}

	nrChan := make(chan *NrMetric)
	quitChan := make(chan struct{})

	go processStats(quitChan, nrChan)

	if config.NewRelicLicenseKey == "" {
		log.Warnf("No New Relic license key... skipping stats reporting")
	} else {
		go processUploads(nrChan)
	}

	select {}
}
