package event

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Emitter interface {
	EmitEvent(Event) error
}

type websocketEmitter struct {
	logURL string

	dialer *websocket.Dialer

	conn  *websocket.Conn
	connL *sync.Mutex
}

func NewWebSocketEmitter(logURL string) Emitter {
	return &websocketEmitter{
		logURL: logURL,

		dialer: &websocket.Dialer{
			// allow detection of failed writes
			//
			// ideally this would be zero, but gorilla uses that to fill in its own
			// default of 4096 :(
			WriteBufferSize: 1,
		},

		connL: new(sync.Mutex),
	}
}

func (e *websocketEmitter) EmitEvent(event Event) error {
	for {
		e.connect()

		err := e.conn.WriteJSON(Message{
			Event: event,
		})
		if err == nil {
			break
		}

		e.close()

		time.Sleep(time.Second)
	}

	return nil
}

func (e *websocketEmitter) connect() {
	e.connL.Lock()
	defer e.connL.Unlock()

	if e.conn != nil {
		return
	}

	var err error

	for {
		e.conn, _, err = e.dialer.Dial(e.logURL, nil)
		if err == nil {
			return
		}

		time.Sleep(time.Second)
	}
}

func (e *websocketEmitter) close() error {
	e.connL.Lock()
	defer e.connL.Unlock()

	if e.conn != nil {
		conn := e.conn
		e.conn = nil
		return conn.Close()
	}

	return nil
}
