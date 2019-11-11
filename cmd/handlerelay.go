package cmd

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/eclesh/welford"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// ***************************************************************************

// readPump pumps messages from the request connection to the hub.
//
// The application runs readPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *Client) readPump(broadcaster bool) {
	defer func() {
		// Ensure that we close the connection:
		defer c.hconn.Close()
		c.hub.unregister <- c
		//read the rest of the request?
	}()

	maxFrameBytes := 1024

	var frameBuffer mutexBuffer

	rawFrame := make([]byte, maxFrameBytes)

	glob := make([]byte, maxFrameBytes)

	frameBuffer.b.Reset() //else we send whole buffer on first flush

	tCh := make(chan int)

	// Read from the buffer, blocking if empty
	go func() {

		for {

			tCh <- 0 //tell the monitoring routine we're alive

			n, err := io.ReadAtLeast(c.hconn, glob, 1)

			if err == nil {

				frameBuffer.mux.Lock()

				_, err = frameBuffer.b.Write(glob[:n])

				frameBuffer.mux.Unlock()

				if err != nil {
					log.Errorf("%v", err) //was Fatal?
					return
				}

			} else {

				return // avoid spinning our wheels

			}
		}
	}()

	for {

		select {

		case <-tCh:

			// do nothing, just received data from buffer

		case <-time.After(1 * time.Millisecond):
			// no new data for >= 1mS weakly implies frame has been fully sent to us
			// this is two orders of magnitude more delay than when reading from
			// non-empty buffer so _should_ be ok, but recheck if errors crop up on
			// lower powered system. Assume am on same computer as capture routine

			//flush buffer to internal send channel
			frameBuffer.mux.Lock()

			n, err := frameBuffer.b.Read(rawFrame)

			frame := rawFrame[:n]

			frameBuffer.b.Reset()

			frameBuffer.mux.Unlock()

			if err == nil && n > 0 {

				if broadcaster {
					t := time.Now()

					c.hub.broadcast <- message{sender: *c, data: frame}

					if c.stats.tx.ns.Count() > 0 {
						c.stats.tx.ns.Add(float64(t.UnixNano() - c.stats.tx.last.UnixNano()))
					} else {
						c.stats.tx.ns.Add(float64(t.UnixNano() - c.stats.connectedAt.UnixNano()))
					}
					c.stats.tx.last = t
					c.stats.tx.size.Add(float64(len(frame)))

				} else {
					log.WithFields(log.Fields{"data": frame, "sender": c, "topic": c.topic}).Warn("Incoming message from non-broadcaster")
				}

			}

		}
	}
}

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Client) writePump(closed <-chan struct{}) {
	ticker := time.NewTicker(pingPeriod)

	defer func() {
		ticker.Stop()
	}()

	for {
		select {
		case message, ok := <-c.send:

			if !ok {
				return
			}

			//write to conn here
			//*bufio.ReadWriter,
			_, err := c.hconn.Write(message.data) //size was n
			c.bufrw.Flush()
			if err != nil {
				return
			}

			size := len(message.data)

			// Add queued chunks to the current websocket message, without delimiter.
			//n := len(c.send)
			//for i := 0; i < n; i++ {
			//	followOnMessage := <-c.send
			//	w.Write(followOnMessage.data)
			//	size += len(followOnMessage.data)
			//}

			t := time.Now()
			if c.stats.rx.ns.Count() > 0 {
				c.stats.rx.ns.Add(float64(t.UnixNano() - c.stats.rx.last.UnixNano()))
			} else {
				c.stats.rx.ns.Add(float64(t.UnixNano() - c.stats.connectedAt.UnixNano()))
			}
			c.stats.rx.last = t
			c.stats.rx.size.Add(float64(size))

		case <-ticker.C:
			// Is there an http equivalent  for long lived connections?
			//c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			//if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
			//	return
			//}
		case <-closed:
			return
		}
	}
}

// serveWs handles websocket requests from the peer.
func serveRelay(closed <-chan struct{}, hub *Hub, w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "webserver doesn't support hijacking", http.StatusInternalServerError)
		return
	}
	fmt.Println("Hijacked!")
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Read only connections were useful for video (crossbar), but can
	// probably be enforced at the originating end - nonetheless,
	// it _may_ come in handy some day, so leave it in.
	// Original comment: where it is explicitly desired not to permit reverse shell access
	// reuse our existing hub which does not know about permissions
	// so enforce in readPump by ignoring messages when the client has
	// no permission to input messages to the crossbar for broadcast
	// i.e. any client connecting to /out/<rest/of/path>
	broadcaster := true
	topic := slashify(r.URL.Path)
	if strings.HasPrefix(topic, "/out/") {
		// we're a receiver-only, so
		// prevent any messages being broadcast from this client
		broadcaster = false
	} else if strings.HasPrefix(topic, "/in/") {
		// we're a sender to receiver only clients, hence
		// convert topic so we write to those receiving clients
		topic = strings.Replace(topic, "/in", "/out", 1)
	} // else do nothing i.e. permit bidirectional messaging at other endpoints

	// initialise statistics
	tx := &Frames{size: welford.New(), ns: welford.New()}
	rx := &Frames{size: welford.New(), ns: welford.New()}
	stats := &Stats{connectedAt: time.Now(), tx: tx, rx: rx}

	client := &Client{hub: hub,
		bufrw:       bufrw,
		hconn:       conn,
		send:        make(chan message, 256),
		topic:       topic,
		broadcaster: broadcaster,
		stats:       stats,
		name:        uuid.New().String(),
		userAgent:   r.UserAgent(),
		remoteAddr:  r.Header.Get("X-Forwarded-For"),
	}
	client.hub.register <- client

	// Allow collection of memory referenced by the caller by doing all work in
	// new goroutines.
	go client.writePump(closed)
	go client.readPump(broadcaster)
}
