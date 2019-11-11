package cmd

import (
	"context"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

func HandleConnections(closed <-chan struct{}, wg *sync.WaitGroup, clientActionsChan chan clientAction, messagesFromMe chan message, port string) {
	hub := newHub()
	go hub.run()

	//this serves a stats page
	http.HandleFunc("/stats", servePage)

	// this handles data relay connections
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveRelay(closed, hub, w, r)
	})

	// this is a websocket connection for listening to stats
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveStats(closed, hub, w, r)
	})

	h := &http.Server{Addr: ":" + port, Handler: nil}

	go func() {
		if err := h.ListenAndServe(); err != nil {
			log.Fatal("ListenAndServe: ", err)
		}
	}()

	<-closed

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.Shutdown(ctx)
	wg.Done()
}
