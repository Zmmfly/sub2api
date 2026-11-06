package traffic

import "testing"

func TestRecordSumsWithAttemptsAndKeepsUnknown(t *testing.T) {
	collector := NewCollector()
	http := collector.Begin(UpstreamResponse, false)
	http.AddOriginal(10)
	http.AddActual(10)
	http.CompleteOriginal()
	http.CompleteActual()
	original, actual := int64(400), int64(90)
	collector.Record(UpstreamResponse, &original, &actual, true)
	collector.Record(UpstreamRequest, nil, &actual, true) // failed-read original stays unknown

	usage := collector.Snapshot()
	response := usage.Upstream.Response
	if response.OriginalBytes == nil || *response.OriginalBytes != 410 || response.ActualBytes == nil || *response.ActualBytes != 100 || !response.Compressed {
		t.Fatalf("response = %+v", response)
	}
	request := usage.Upstream.Request
	if request.OriginalBytes != nil || request.ActualBytes == nil || *request.ActualBytes != 90 {
		t.Fatalf("request = %+v", request)
	}

	collector.Finalize()
	late := int64(1)
	collector.Record(UpstreamResponse, &late, &late, false)
	if got := *collector.Snapshot().Upstream.Response.ActualBytes; got != 100 {
		t.Fatalf("record after finalize changed the snapshot: %d", got)
	}
}
