// Command measure-feed is what test/e2e/measure.sh's servers are fed: a
// target Prometheus scrapes, whose numbers are functions of the clock, and
// log lines sent to Loki every second, the same. What a rule over them
// fires is then known to the second, and the same on every run.
//
// The metrics, at /metrics: fv_spiky, 100 for the first 15s of every two
// minutes and 1 otherwise; fv_dippy, the other way round; fv_gappy, there
// for the first minute of every 15 and absent the rest.
//
// The logs, each stamped a fraction of a second after the whole second it is
// for (.95 at first), and sent before it, so none arrives late. A Loki group
// runs a fraction of a second past a second, and a line stamped in that
// fraction is in what it reads but not in what a replay at the second reads:
// with -ruler, the feed asks Loki's ruler when each group last ran, and
// stamps the lines past the latest fraction of them all, half way to the next
// second (stamp). The shop's info line, a second; the shop's
// errors, two a second for 40s of every three minutes; the cart's, one a
// second for the first minute of every five; the api's latency, 900 for
// 20s of every two minutes and 100 to 106 otherwise; a stream, gappy, there
// for two minutes of every ten.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

func main() {
	listen := flag.String("listen", ":9100", "address to serve /metrics on")
	loki := flag.String("loki", "", "Loki's URL; the logs are sent to it when given")
	ruler := flag.Bool("ruler", false, "stamp the lines past the fraction of a second Loki's groups run at")
	flag.Parse()
	if *loki != "" {
		if *ruler {
			go watch(*loki + "/prometheus/api/v1/rules")
		}
		go push(*loki + "/loki/api/v1/push")
	}
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		s := time.Now().Unix()
		spiky, dippy := 1, 100
		if s%120 < 15 {
			spiky, dippy = 100, 1
		}
		fmt.Fprintf(w, "# TYPE fv_spiky gauge\nfv_spiky %d\n# TYPE fv_dippy gauge\nfv_dippy %d\n", spiky, dippy)
		if s%900 < 60 {
			fmt.Fprint(w, "# TYPE fv_gappy gauge\nfv_gappy 1\n")
		}
	})
	srv := &http.Server{Addr: *listen, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// fraction is the nanoseconds past its second a line is stamped.
var fraction atomic.Int64

func init() { fraction.Store(950_000_000) }

// stamp is the fraction of a second, in nanoseconds, to stamp lines at when
// groups last ran at times: past the latest fraction of them, half way to the
// next second, but not past .99. With none, .95.
func stamp(last []time.Time) int64 {
	var most int64 = -1
	for _, t := range last {
		if t.Year() > 1970 {
			most = max(most, int64(t.Nanosecond()))
		}
	}
	if most < 0 {
		return 950_000_000
	}
	return min(most+(1_000_000_000-most)/2, 990_000_000)
}

// watch asks Loki's ruler every half minute when its groups last ran, and
// stamps by it.
func watch(url string) {
	c := &http.Client{Timeout: 5 * time.Second}
	for {
		resp, err := c.Get(url)
		if err == nil {
			var r struct {
				Data struct {
					Groups []struct {
						LastEvaluation time.Time `json:"lastEvaluation"`
					} `json:"groups"`
				} `json:"data"`
			}
			err = json.NewDecoder(resp.Body).Decode(&r)
			resp.Body.Close()
			if err == nil {
				var last []time.Time
				for _, g := range r.Data.Groups {
					last = append(last, g.LastEvaluation)
				}
				if f := stamp(last); fraction.Swap(f) != f {
					log.Printf("lines stamped at .%03d", f/1_000_000)
				}
			}
		}
		if err != nil {
			log.Print(err)
		}
		time.Sleep(30 * time.Second)
	}
}

type stream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// lines is what is logged at second s, as streams.
func lines(s int64) []stream {
	ns := fmt.Sprintf("%d%09d", s, fraction.Load())
	one := func(app, level string, msgs ...string) stream {
		st := stream{Stream: map[string]string{"app": app, "level": level}}
		for _, m := range msgs {
			st.Values = append(st.Values, [2]string{ns, m})
		}
		return st
	}
	out := []stream{
		one("shop", "info", "msg=ok path=/ user=anon"),
		one("api", "info", fmt.Sprintf("msg=done latency_ms=%d", latency(s))),
	}
	if s%180 < 40 {
		out = append(out, one("shop", "error", "msg=error code=500 path=/checkout n=1", "msg=error code=500 path=/checkout n=2"))
	}
	if s%300 < 60 {
		out = append(out, one("cart", "error", "msg=error code=500 path=/cart"))
	}
	if s%600 < 120 {
		out = append(out, one("gappy", "info", "msg=here"))
	}
	return out
}

func latency(s int64) int64 {
	if s%120 < 20 {
		return 900
	}
	return 100 + s%7
}

// push sends the logs of every second, a second ahead of their stamp.
func push(url string) {
	c := &http.Client{Timeout: 5 * time.Second}
	for {
		s := time.Now().Unix() + 1
		b, err := json.Marshal(map[string][]stream{"streams": lines(s)})
		if err != nil {
			log.Fatal(err)
		}
		if resp, err := c.Post(url, "application/json", bytes.NewReader(b)); err != nil {
			log.Print(err)
		} else {
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				log.Printf("loki: %s", resp.Status)
			}
		}
		time.Sleep(time.Until(time.Unix(s, 0)))
	}
}
