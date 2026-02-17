# gFly Websocket

gFly WebSocket - A computer communications protocol providing full-duplex (bidirectional) communication channels over a single, long-lived TCP connection

    Copyright © 2023, gFly
    https://www.gFly.dev
    All rights reserved.

# Building a WebSocket App with `github.com/gflydev/websocket`

This guide walks through building a real-time chat application using the `gflydev/websocket` library on top of the [gFly](https://www.gfly.dev) web framework.

---

## Installation

```bash
go get -u github.com/gflydev/websocket@v1.0.0
go get -u github.com/gflydev/core
```

---

## Architecture Overview

```
Client (browser)
    │  WebSocket (ws://)
    ▼
WSHandler (gFly route handler)
    │  upgrader.Upgrade()
    ▼
Client struct ──► Hub (broadcast room)
    │                 │
    ├─ readPump()     ├─ register channel
    └─ writePump()    ├─ unregister channel
                      └─ broadcast channel
```

The key components are:

| Component | Role |
|-----------|------|
| `FastHTTPUpgrader` | Upgrades an HTTP request to a WebSocket connection |
| `*websocket.Conn` | The live bidirectional connection to one client |

---

## Step 1 – Create the Upgrader

```go
import "github.com/gflydev/websocket"

var upgrader = websocket.FastHTTPUpgrader{
    ReadBufferSize:  10240,
    WriteBufferSize: 10240,
    CheckOrigin: func(r *fasthttp.RequestCtx) bool {
        // Validate the Origin header here.
        // Return true to allow all origins (development only).
        return true
    },
}
```

`FastHTTPUpgrader` is the server-side handshake handler. Important fields:

| Field | Description |
|-------|-------------|
| `ReadBufferSize` / `WriteBufferSize` | I/O buffer sizes in bytes |
| `CheckOrigin` | Return `false` to reject cross-origin connections |
| `EnableCompression` | Enable per-message deflate (RFC 7692) |
| `HandshakeTimeout` | Max duration to complete the WS handshake |

---

## Step 2 – Create a Hub

A Hub is a chat room that keeps track of connected clients and forwards messages.

```go
type Hub struct {
    clients    map[*Client]bool
    broadcast  chan []byte
    register   chan *Client
    unregister chan *Client
    name       string
}

func newHub(name string) *Hub {
    return &Hub{
        broadcast:  make(chan []byte),
        register:   make(chan *Client),
        unregister: make(chan *Client),
        clients:    make(map[*Client]bool),
        name:       name,
    }
}

func (h *Hub) run() {
    for {
        select {
        case client := <-h.register:
            h.clients[client] = true

        case client := <-h.unregister:
            if _, ok := h.clients[client]; ok {
                delete(h.clients, client)
            }

        case message := <-h.broadcast:
            for client := range h.clients {
                select {
                case client.send <- message:
                default:
                    close(client.send)
                    delete(h.clients, client)
                }
            }
        }
    }
}
```

Start the hub in a goroutine before accepting connections:

```go
hub := newHub("General")
go hub.run()
```

---

## Step 3 – Define the Client

A Client sits between a `websocket.Conn` and its Hub.

```go
const (
    writeWait      = 10 * time.Second
    pongWait       = 60 * time.Second
    pingPeriod     = (pongWait * 9) / 10
    maxMessageSize = 10240
)

type Client struct {
    hub  *Hub
    conn *websocket.Conn
    send chan []byte // buffered outbound messages
    id   string
}
```

### readPump – client → hub

```go
func (c *Client) readPump() {
    defer func() {
        c.hub.unregister <- c
        c.conn.Close()
    }()

    c.conn.SetReadLimit(maxMessageSize)
    c.conn.SetReadDeadline(time.Now().Add(pongWait))
    c.conn.SetPongHandler(func(string) error {
        return c.conn.SetReadDeadline(time.Now().Add(pongWait))
    })

    for {
        _, message, err := c.conn.ReadMessage()
        if err != nil {
            if websocket.IsUnexpectedCloseError(err,
                websocket.CloseGoingAway,
                websocket.CloseAbnormalClosure) {
                log.Printf("error: %v", err)
            }
            break
        }
        c.hub.broadcast <- bytes.TrimSpace(message)
    }
}
```

### writePump – hub → client

```go
func (c *Client) writePump() {
    ticker := time.NewTicker(pingPeriod)
    defer func() {
        ticker.Stop()
        c.conn.Close()
    }()

    for {
        select {
        case message, ok := <-c.send:
            c.conn.SetWriteDeadline(time.Now().Add(writeWait))
            if !ok {
                c.conn.WriteMessage(websocket.CloseMessage, []byte{})
                return
            }
            w, err := c.conn.NextWriter(websocket.TextMessage)
            if err != nil {
                return
            }
            w.Write(message)
            // flush any queued messages
            for n := len(c.send); n > 0; n-- {
                w.Write([]byte{'\n'})
                w.Write(<-c.send)
            }
            if err := w.Close(); err != nil {
                return
            }

        case <-ticker.C:
            c.conn.SetWriteDeadline(time.Now().Add(writeWait))
            if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
                return
            }
        }
    }
}
```

Key points:
- **One goroutine per direction** – `readPump` blocks reading; `writePump` blocks writing. This is required by the library.
- **Ping/Pong keepalive** – the ticker sends `PingMessage` every `pingPeriod`; the pong handler extends the read deadline.
- **Backpressure** – when the Hub's `default` branch fires (send buffer full), the client is dropped.

---

## Step 4 – ServeWS (the upgrade entry point)

```go
func ServeWS(ctx *fasthttp.RequestCtx) {
    err := upgrader.Upgrade(ctx, func(conn *websocket.Conn) {
        client := &Client{
            hub:  hub,
            conn: conn,
            send: make(chan []byte, 256),
            id:   conn.RemoteAddr().String(),
        }
        client.hub.register <- client

        go client.writePump() // must run concurrently
        client.readPump()     // blocks until disconnect
    })
    if err != nil {
        log.Printf("upgrade error: %v", err)
    }
}
```

`upgrader.Upgrade` performs the HTTP→WS handshake and then calls your handler with the live `*websocket.Conn`.

---

## Step 5 – Wire up the gFly Route

```go
// internal/websocket.go
type WSHandler struct{ core.Page }

func (h *WSHandler) Handle(c *core.Ctx) error {
    ServeWS(c.Root()) // c.Root() returns *fasthttp.RequestCtx
    return nil
}

// cmd/web/main.go
func main() {
    core.Bootstrap()
    app := core.New()
    app.RegisterRouter(func(g core.IFly) {
        g.GET("/ws", &WSHandler{})
    })
    app.Run()
}
```

Clients connect with:

```
ws://localhost:8080/ws
ws://localhost:8080/ws?channel=sports   // join a specific room
```

---

## Message Formats

### Plain text (minimal)

```
Hello world
```

### Structured JSON – send a message

```json
{
  "metadata": { "version": "1.0", "timestamp": "2026-02-18T00:00:00Z" },
  "channel":  { "id": "general", "type": "group" },
  "action": {
    "type": "send_message",
    "data": {
      "message": {
        "id":        "abc123",
        "sender_id": "user42",
        "timestamp": "2026-02-18T00:00:00Z",
        "type":      "text",
        "content":   { "text": "Hello!" },
        "status":    "sent",
        "reactions": []
      }
    }
  }
}
```

### Switch channel

```json
{
  "action": {
    "type": "switch_channel",
    "data": { "channel_id": "sports" }
  }
}
```

### List channels

```json
{ "action": { "type": "list_channels", "data": {} } }
```

### Authenticate

```json
{
  "action": {
    "type": "user_auth",
    "data": { "username": "alice", "password": "secret" }
  }
}
```

---

## Supported Action Types

| Action type | Direction | Description |
|-------------|-----------|-------------|
| `send_message` | client → server | Broadcast a message to the current room |
| `switch_channel` | client → server | Move to a different room without reconnecting |
| `list_channels` | client → server | Request the list of available rooms |
| `create_channel` | client → server | Create a new room |
| `user_auth` | client → server | Authenticate with username/password |
| `list_channels` | server → client | Response with `ChannelListData` |
| `create_channel` | server → client | Confirmation of channel creation |
| `user_auth` | server → client | Auth success/failure response |

---

## Multi-Room Support

The application supports multiple independent rooms via a `Manager` that owns a pool of Hubs.

```go
type Manager struct {
    poolHub map[string]*Hub
}

// Get an existing hub
hub := manager.GetHub("sports")

// Create a new hub for a room
hub = manager.CreateChannelHub("sports", "Sports Talk")
go hub.run()

// Switch a connected client to a different room
client.SwitchChannel(newHub)
// This unregisters from the current hub and registers with the new one
// without closing the WebSocket connection.
```

---

## WebSocket Constants (from the library)

```go
// Message types
websocket.TextMessage   // = 1
websocket.BinaryMessage // = 2
websocket.CloseMessage  // = 8
websocket.PingMessage   // = 9
websocket.PongMessage   // = 10

// Close codes (RFC 6455 §11.7)
websocket.CloseNormalClosure    // 1000
websocket.CloseGoingAway        // 1001
websocket.CloseAbnormalClosure  // 1006

// Helpers
websocket.IsUnexpectedCloseError(err, codes...) bool
websocket.FastHTTPIsWebSocketUpgrade(ctx) bool
```

---

## Quick Start Checklist

1. `go get github.com/gflydev/websocket@v1.0.0`
2. Create an `upgrader` (`FastHTTPUpgrader`) with your `CheckOrigin` logic.
3. Create one or more `Hub` instances and call `go hub.run()`.
4. In your HTTP handler, call `upgrader.Upgrade(ctx, handler)`.
5. Inside the handler: create a `Client`, register it with the hub, then launch `writePump` as a goroutine and call `readPump` on the same goroutine.
6. Register the route with `app.RegisterRouter`.
