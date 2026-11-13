package prom

// What a range answer's labels, and a long token, can make lapilli hold or
// say.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A hostile API sends the same series of 300 tiny labels again and again:
// it gzips to almost nothing, and each copy is a map of 300 entries, some
// 12 times its decoded size and 2,000 times what was on the wire. An
// answer's labels are bounded (maxAnswerLabels, as labelCost counts them;
// 16 MiB here, 128 MiB as built): past that, it can't be read, over any
// range, and isn't asked again over a shorter one, which would hold the
// same series.
func TestAnswerLabelsBounded(t *testing.T) {
	was := maxAnswerLabels
	maxAnswerLabels = 16 << 20
	t.Cleanup(func() { maxAnswerLabels = was })
	const series, labels = 5000, 300
	var one bytes.Buffer
	one.WriteString(`{"metric":{`)
	for i := range labels {
		if i > 0 {
			one.WriteByte(',')
		}
		one.WriteString(`"a` + strconv.Itoa(i) + `":""`)
	}
	one.WriteString(`},"values":[[1759276800,"1"]]}`)
	var b bytes.Buffer
	b.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[`)
	for s := range series {
		if s > 0 {
			b.WriteByte(',')
		}
		b.Write(one.Bytes())
	}
	b.WriteString(`]}}`)
	body := b.Bytes()
	srv, wire := hostile(t, body)
	t0 := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	c := New(srv.URL, "t")
	before := heapAfterGC()
	var got []Series
	var err error
	peak := peakDuring(func() { got, err = c.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute) })
	runtime.KeepAlive(got)
	ratio := float64(peak-before) / float64(len(body))
	t.Logf("wire %d KiB, decoded %d MiB, %d series kept, heap peak +%d MiB (%.1fx decoded), err %v",
		wire.Load()>>10, len(body)>>20, len(got), (peak-before)>>20, ratio, err)
	if ratio > 4 {
		t.Errorf("%d KiB on the wire, %d MiB decoded, took the heap up %d MiB (%.1fx): labels of a series aren't bounded as its samples are",
			wire.Load()>>10, len(body)>>20, (peak-before)>>20, ratio)
	}
	if err == nil || !strings.Contains(err.Error(), "too many, or too wide, series to read") || wire.Load() > int64(len(gzipped(t, body)))+1 {
		t.Errorf("asked %d bytes: %v", wire.Load(), err)
	}
}

// An unreadable sample is quoted with the token taken out before it is cut
// to 64 bytes: cut first, a token longer than the cut (a GCP access token,
// ya29.…, an Azure AD or a Grafana-issued JWT, hundreds of bytes) would no
// longer be whole, and its first ~60 bytes would stay in the error, which
// --markdown puts in a pull request's comment.
func TestUnreadableSampleLongToken(t *testing.T) {
	token := "ya29." + strings.Repeat("A1b2C3d4E5", 20) // 205 bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		echo := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1759276800,"` + echo + `"]]}]}}`))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "t", WithToken(func(context.Context) (string, error) { return token, nil }))
	t0 := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	_, err := c.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
	if err == nil {
		t.Fatal("an unreadable sample was read")
	}
	if strings.Contains(err.Error(), token[:40]) {
		t.Errorf("the error holds the first bytes of the token the request carried: %v", err)
	}
}
