package rewebsocket

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketReconnect(t *testing.T) {
	srv := httptest.NewServer(echoer(t))
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http", "ws", 1)

	readCh := make(chan []byte)
	var reconnectCount int
	c := WebSocketTextClient{
		OnReadMessage: func(cancel chan struct{}, msg []byte) {
			readCh <- msg
		},
		OnReopen: func(cancel chan struct{}) (string, http.Header, error) {
			reconnectCount++
			return wsURL, nil, nil
		},
		logln: t.Log,
	}
	err := c.Open(wsURL, nil)
	if err != nil {
		t.Fatal("client dial:", err)
	}
	defer c.Close()

	var receivedEcho bool

	for i := 0; i < 10; i++ {
		select {
		case message := <-readCh:
			t.Logf("client recv: %s", message)
			receivedEcho = true
			goto End
		case <-time.After(200 * time.Millisecond):
			t.Log("read timed out")
			err = c.WriteTextMessage([]byte(`hello`))
			if err != nil {
				t.Log("client write:", err)
				continue
			}
			t.Log("client write SUCCESS")
		}
	}

End:
	if !receivedEcho {
		t.Fatal("did not receive echo from server")
	}
	if reconnectCount < 3 {
		t.Fatal("did not reconnect enough times")
	}
}

func echoer(t *testing.T) http.HandlerFunc {
	var upgrader = websocket.Upgrader{}
	var failureCounter int

	return func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Log("server upgrade:", err)
			return
		}
		defer c.Close()
		failureCounter++

		// fake connection issue the first three times
		if failureCounter <= 3 {
			return
		}

		for {
			mt, message, err := c.ReadMessage()

			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					t.Log("client disconnected")
					break
				}
				t.Log("server read:", err)
				break
			}
			t.Logf("server recv: %s", message)
			err = c.WriteMessage(mt, message)
			if err != nil {
				t.Log("server write:", err)
				break
			}
		}
	}
}
