// server.go
package main

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// --- GORM models ---
type User struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"uniqueIndex;size:100;not null"`
	CreatedAt time.Time
}

type Message struct {
	ID        uint   `gorm:"primaryKey"`
	UserID    uint   `gorm:"index;not null"`
	User      User   `gorm:"foreignKey:UserID"`
	Content   string `gorm:"type:text;not null"`
	CreatedAt time.Time
}

// global GORM handle
var db *gorm.DB

func main() {
	// 1) Hard-coded DSN instead of env var:
	dsn := "postgres://myuser:mypass@localhost:5432/chatdb?sslmode=disable"

	// 2) initialize GORM
	gormConfig := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	}
	var err error
	db, err = gorm.Open(postgres.Open(dsn), gormConfig)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}

	// 3) start the WebSocket hub
	hub := newHub()
	go hub.run()

	// 4) HTTP handlers
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})
	http.Handle("/", http.FileServer(http.Dir("./public")))

	addr := ":8080"
	log.Printf("Server running on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// --- Hub: manages all active clients and broadcasts ---
type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

func (h *Hub) run() {
	for {
		select {
		case c := <-h.register:
			var msgs []Message
			if err := db.
				Preload("User").
				Order("created_at DESC").
				Limit(50).
				Find(&msgs).Error; err != nil {
				log.Println("history load:", err)
			} else {
				for i := len(msgs) - 1; i >= 0; i-- {
					m := msgs[i]
					line := []byte(
						m.CreatedAt.Format("15:04") + " " +
							m.User.Name + ": " + m.Content,
					)
					c.send <- line
				}
			}
			h.clients[c] = true

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
			}

		case msg := <-h.broadcast:
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					close(c.send)
					delete(h.clients, c)
				}
			}
		}
	}
}

// --- Client: a middleman between WebSocket and Hub ---
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		text := string(raw)

		parts := strings.SplitN(text, ":", 2)
		userName, content := "anon", text
		if len(parts) == 2 {
			userName = strings.TrimSpace(parts[0])
			content = strings.TrimSpace(parts[1])
		}

		user := User{Name: userName}
		if err := db.FirstOrCreate(&user, User{Name: userName}).Error; err != nil {
			log.Println("user upsert:", err)
		}

		msg := Message{UserID: user.ID, Content: content}
		if err := db.Create(&msg).Error; err != nil {
			log.Println("message insert:", err)
		}

		line := []byte(
			time.Now().Format("15:04") + " " +
				userName + ": " + content,
		)
		c.hub.broadcast <- line
	}
}

func (c *Client) writePump() {
	defer c.conn.Close()
	for msg := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			break
		}
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	client := &Client{
		hub:  hub,
		conn: conn,
		send: make(chan []byte, 256),
	}
	hub.register <- client

	go client.writePump()
	client.readPump()
}
