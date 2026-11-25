// Command keeper is the webhook receiver of test/e2e/routing.sh: it keeps
// every notification an Alertmanager posts to it, with the time it got it,
// and gives them back.
//
//	POST /INSTANCE/RECEIVER/INDEX   keeps the body (a webhook notification)
//	GET  /notifications             every kept one, a JSON array, in order
//	GET  /ready                     200
//
// INSTANCE says which Alertmanager sent it, RECEIVER the receiver's name and
// INDEX its webhook's place among the receiver's integrations, from 0.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type kept struct {
	At          time.Time       `json:"at"`
	Instance    string          `json:"instance"`
	Receiver    string          `json:"receiver"`
	Integration int             `json:"integration"`
	Body        json.RawMessage `json:"body"`
}

func main() {
	addr := flag.String("listen", ":8080", "address to listen on")
	flag.Parse()
	var (
		mu  sync.Mutex
		all = []kept{}
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /notifications", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(all)
	})
	mux.HandleFunc("POST /{instance}/{receiver}/{index}", func(w http.ResponseWriter, r *http.Request) {
		at := time.Now().UTC()
		i, err := strconv.Atoi(r.PathValue("index"))
		body, rerr := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil || rerr != nil || !json.Valid(body) {
			http.Error(w, "bad notification", http.StatusBadRequest)
			return
		}
		k := kept{At: at, Instance: r.PathValue("instance"), Receiver: r.PathValue("receiver"), Integration: i, Body: body}
		mu.Lock()
		all = append(all, k)
		mu.Unlock()
		log.Printf("%s %s/%s/%d, %d bytes", at.Format(time.RFC3339Nano), k.Instance, k.Receiver, i, len(body))
	})
	log.Fatal((&http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}
