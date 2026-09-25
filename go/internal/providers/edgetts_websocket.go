package providers

import (
	"context"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// edgeTTSDialer is the websocket dialer core's Edge TTS opens its synthesis
// socket with. Core has no websocket client, and the gateway's is
// gorilla/websocket, which dials as the gateway's own Edge TTS client did:
// through the environment's proxy, offering compression, with a handshake
// bounded by timeout as well as by ctx. A refused handshake returns
// gorilla's response beside the error, whose Date core learns the
// service's clock from.
func edgeTTSDialer(timeout time.Duration) coreproviders.WebSocketDialer {
	return func(ctx context.Context, url string, header http.Header, subprotocols []string) (coreproviders.WebSocketConn, *http.Response, error) {
		dialer := websocket.Dialer{
			Proxy: http.ProxyFromEnvironment, HandshakeTimeout: timeout,
			EnableCompression: true, Subprotocols: subprotocols,
		}
		conn, response, err := dialer.DialContext(ctx, url, header)
		if err != nil {
			return nil, response, err
		}
		return edgeTTSConn{conn: conn}, response, nil
	}
}

// edgeTTSConn is a gorilla connection as core's Edge TTS uses one. A write
// or a read waits no longer than its context's deadline, which is the
// exchange's; without one it waits for the connection. Core closes the
// connection from another goroutine when the exchange ends, which gorilla
// permits beside a read or a write, and a second Close only reports that
// the connection is closed.
type edgeTTSConn struct{ conn *websocket.Conn }

var _ coreproviders.WebSocketConn = edgeTTSConn{}

func (c edgeTTSConn) WriteText(ctx context.Context, data []byte) error {
	deadline, _ := ctx.Deadline()
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c edgeTTSConn) Read(ctx context.Context) (int, []byte, error) {
	deadline, _ := ctx.Deadline()
	if err := c.conn.SetReadDeadline(deadline); err != nil {
		return 0, nil, err
	}
	return c.conn.ReadMessage()
}

func (c edgeTTSConn) Close() error { return c.conn.Close() }
