package relay

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ihavespoons/transitive/internal/protocol"
)

// MessageHandler is called when a message arrives from the relay (from mobile).
type MessageHandler func(env *protocol.Envelope)

// Client maintains a WebSocket connection to the Cloudflare relay.
type Client struct {
	relayURL string
	agentID  string
	secret   string
	key      []byte
	handler  MessageHandler

	mu   sync.Mutex
	conn *websocket.Conn
	done chan struct{}
}

func NewClient(relayURL, agentID, secret string, handler MessageHandler) (*Client, error) {
	key, err := DeriveKey(secret, agentID)
	if err != nil {
		return nil, fmt.Errorf("derive e2e key: %w", err)
	}
	return &Client{
		relayURL: relayURL,
		agentID:  agentID,
		secret:   secret,
		key:      key,
		handler:  handler,
		done:     make(chan struct{}),
	}, nil
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

// relayInternalTypes are envelope types that the relay needs to inspect,
// so they must NOT be encrypted.
var relayInternalTypes = map[string]bool{
	"device.token":              true,
	"notification.preferences":  true,
	protocol.TypeProxyRegister:   true,
	protocol.TypeProxyUnregister: true,
	protocol.TypeProxyResponse:   true,
}

// e2eEnvelope is the outer wrapper sent over the wire for encrypted messages.
type e2eEnvelope struct {
	E2E  bool    `json:"e2e"`
	CT   string  `json:"ct"`
	IV   string  `json:"iv"`
	Hint e2eHint `json:"hint"`
}

// e2eHint contains minimal unencrypted metadata for the relay (e.g. push routing).
type e2eHint struct {
	Type       string `json:"type"`
	InstanceID string `json:"instance_id,omitempty"`
}

// Send sends a protocol envelope to the relay (to mobile).
func (c *Client) Send(env *protocol.Envelope) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	var data []byte

	if relayInternalTypes[env.Type] {
		// Send relay-internal messages as plain JSON envelopes.
		var err error
		data, err = json.Marshal(env)
		if err != nil {
			return fmt.Errorf("marshal envelope: %w", err)
		}
	} else {
		// Encrypt all other messages with E2E.
		plaintext, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("marshal envelope for encryption: %w", err)
		}

		ct, iv, err := Encrypt(c.key, plaintext)
		if err != nil {
			return fmt.Errorf("encrypt envelope: %w", err)
		}

		// Extract instance_id from payload for the hint.
		var payloadHint struct {
			InstanceID string `json:"instance_id"`
		}
		if len(env.Payload) > 0 {
			_ = json.Unmarshal(env.Payload, &payloadHint)
		}

		wrapper := e2eEnvelope{
			E2E: true,
			CT:  ct,
			IV:  iv,
			Hint: e2eHint{
				Type:       env.Type,
				InstanceID: payloadHint.InstanceID,
			},
		}

		data, err = json.Marshal(wrapper)
		if err != nil {
			return fmt.Errorf("marshal e2e wrapper: %w", err)
		}
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

		// Check if this is an E2E encrypted message.
		var peek struct {
			E2E bool   `json:"e2e"`
			CT  string `json:"ct"`
			IV  string `json:"iv"`
		}
		if err := json.Unmarshal(message, &peek); err == nil && peek.E2E {
			// Decrypt the inner envelope.
			plaintext, err := Decrypt(c.key, peek.CT, peek.IV)
			if err != nil {
				log.Printf("relay e2e decrypt error: %v", err)
				continue
			}
			if err := json.Unmarshal(plaintext, &env); err != nil {
				log.Printf("relay invalid decrypted message: %v", err)
				continue
			}
		} else {
			// Plain (unencrypted) message -- backward compatibility.
			if err := json.Unmarshal(message, &env); err != nil {
				log.Printf("relay invalid message: %v", err)
				continue
			}
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
