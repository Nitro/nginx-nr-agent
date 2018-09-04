package main

import (
	"testing"

	httpmock "gopkg.in/jarcoal/httpmock.v1"
)

func NginxStatusReponseString() string {
	return `Active connections: 2
server accepts handled requests
 31 30 42
Reading: 0 Writing: 10 Waiting: 1`
}

func Test_GetStats(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	httpmock.RegisterResponder("GET", "http://localhost/status",
		httpmock.NewStringResponder(200, NginxStatusReponseString()))
	metric, _ := GetStats("http://localhost/status")

	var metricTests = []struct {
		want int64
		got  int64
	}{
		{metric.Connections, 2},
		{metric.Accepts, 31},
		{metric.Handled, 30},
		{metric.Requests, 42},
		{metric.Reading, 0},
		{metric.Writing, 10},
		{metric.Waiting, 1},
	}

	for _, input := range metricTests {
		if input.want != input.got {
			t.Errorf("Want %d, Got %d", input.want, input.got)
		}
	}

}
