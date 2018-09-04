package main

import (
	"testing"

	httpmock "gopkg.in/jarcoal/httpmock.v1"
)

func NginxStatusReponseString() {
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
	if metric.Connections != 2 {
		t.Errorf("Want %d, Got %d", 2, metric.Connections)
	}
	if metric.Accepts != 31 {
		t.Errorf("Want %d, Got %d", 31, metric.Accepts)
	}
	if metric.Handled != 30 {
		t.Errorf("Want %d, Got %d", 30, metric.Handled)
	}
	if metric.Reading != 0 {
		t.Errorf("Want %d, Got %d", 0, metric.Reading)
	}
	if metric.Writing != 10 {
		t.Errorf("Want %d, Got %d", 10, metric.Writing)
	}
	if metric.Waiting != 1 {
		t.Errorf("Want %d, Got %d", 1, metric.Waiting)
	}

}
