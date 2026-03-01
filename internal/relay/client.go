package relay

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sync"
	"time"

	"github.com/ihavespoons/transitive/internal/protocol"
	"github.com/gorilla/websocket"
)

// MessageHandler is called when a message arrives from the relay (from mobile).
type MessageHandler func(env *protocol.Envelope)

// Client maintains a WebSocket connection to the Cloudflare relay.
type Client struct {
	relayURL string
	agentID  string
	secret   string
	handler  MessageHandler

	mu   sync.Mutex
	conn *websocket.Conn
	done chan struct{}
}

func NewClient(relayURL, agentID, secret string, handler MessageHandler) *Client {
	return &Client{
		relayURL: relayURL,
		agentID:  agentID,
		secret:   secret,
		handler:  handler,
		done:     make(chan struct{}),
	}
}

// Connect establishes the WebSocket connection to the relay.
func (c *Client) Connect() error {
	u, err := url.Parse(c.relayURL)
	if err != nil {
		return fmt.Errorf("parse relay URL: %w", err)
	}

	u.Path = "/connect"
	q := u.Query()
	q.Set("agent_id", c.agentID)
	q.Set("secret", c.secret)
	u.RawQuery = q.Encode()

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	go c.readLoop()
	go c.pingLoop()

	log.Printf("connected to relay at %s", c.relayURL)
	return nil
}

// Send sends a protocol envelope to the relay (to mobile).
func (c *Client) Send(env *protocol.Envelope) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// Close shuts down the connection.
func (c *Client) Close() {
	close(c.done)
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()
}

func (c *Client) readLoop() {
	defer func() {
		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
		}
		c.mu.Unlock()
	}()

	for {
		select {
		case <-c.done:
			return
		default:
		}

		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()

		if conn == nil {
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("relay read error: %v", err)
			go c.reconnect()
			return
		}

		log.Printf("relay received: %s", string(message))

		var env protocol.Envelope
		if err := json.Unmarshal(message, &env); err != nil {
			log.Printf("relay invalid message: %v", err)
			continue
		}

		if c.handler != nil {
			c.handler(&env)
		}
	}
}

func (c *Client) pingLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.mu.Lock()
			if c.conn != nil {
				c.conn.WriteMessage(websocket.PingMessage, nil)
			}
			c.mu.Unlock()
		}
	}
}

func (c *Client) reconnect() {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-c.done:
			return
		default:
		}

		log.Printf("reconnecting to relay in %v...", backoff)
		time.Sleep(backoff)

		if err := c.Connect(); err != nil {
			log.Printf("reconnect failed: %v", err)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		return
	}
}
