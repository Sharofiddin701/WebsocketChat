package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// Server strukturasi
type Server struct {
	conns    map[*websocket.Conn]*Client
	mu       sync.RWMutex
	clients  map[string]*Client
	rooms    map[string]*Room
	history  map[string][]Message
	maxConns int
}

// Client strukturasi
type Client struct {
	conn     *websocket.Conn
	username string
	room     string
	isAdmin  bool
	lastSeen time.Time
}

// Room strukturasi
type Room struct {
	name       string
	clients    map[string]*Client
	isPrivate  bool
	owner      string
	maxClients int
	createTime time.Time
}

// Message strukturasi
type Message struct {
	Type      string    `json:"type"`
	Username  string    `json:"username"`
	Content   string    `json:"content"`
	Time      string    `json:"time"`
	Room      string    `json:"room"`
	Private   bool      `json:"private"`
	To        string    `json:"to,omitempty"`
	From      string    `json:"from,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Yangi server yaratish
func NewServer() *Server {
	return &Server{
		conns:    make(map[*websocket.Conn]*Client),
		clients:  make(map[string]*Client),
		rooms:    make(map[string]*Room),
		history:  make(map[string][]Message),
		maxConns: 100, // Maksimal ulanishlar soni
	}
}

// Xonani yaratish
func (s *Server) createRoom(name string, owner string, isPrivate bool) *Room {
	room := &Room{
		name:       name,
		clients:    make(map[string]*Client),
		isPrivate:  isPrivate,
		owner:      owner,
		maxClients: 50,
		createTime: time.Now(),
	}
	s.rooms[name] = room
	s.history[name] = make([]Message, 0)
	return room
}

// WebSocket ulanishni boshqarish
func (s *Server) handleWS(ws *websocket.Conn) {
	defer ws.Close()

	// Ulangan mijozlar sonini tekshirish
	s.mu.Lock()
	if len(s.conns) >= s.maxConns {
		s.mu.Unlock()
		websocket.JSON.Send(ws, Message{
			Type:    "error",
			Content: "Server is at maximum capacity",
		})
		return
	}
	s.mu.Unlock()

	// Autentifikatsiya
	var authMsg Message
	if err := websocket.JSON.Receive(ws, &authMsg); err != nil {
		log.Println("Auth error:", err)
		return
	}

	if authMsg.Type != "auth" || !isValidUsername(authMsg.Username) {
		websocket.JSON.Send(ws, Message{
			Type:    "error",
			Content: "Invalid authentication",
		})
		return
	}

	// Foydalanuvchi mavjudligini tekshirish
	s.mu.Lock()
	if _, exists := s.clients[authMsg.Username]; exists {
		s.mu.Unlock()
		websocket.JSON.Send(ws, Message{
			Type:    "error",
			Content: "Username already taken",
		})
		return
	}

	// Yangi mijoz yaratish
	client := &Client{
		conn:     ws,
		username: authMsg.Username,
		isAdmin:  false,
		lastSeen: time.Now(),
	}
	s.conns[ws] = client
	s.clients[authMsg.Username] = client
	s.mu.Unlock()

	// Ulanish haqida xabar
	s.broadcast(Message{
		Type:      "system",
		Content:   fmt.Sprintf("%s joined the chat", authMsg.Username),
		Time:      time.Now().Format("15:04:05"),
		Timestamp: time.Now(),
	})

	// Asosiy xabarlar aylanishi
	s.readLoop(client)

	// Foydalanuvchi chiqib ketganda
	s.handleDisconnect(client)
}

// Foydalanuvchi nomini tekshirish
func isValidUsername(username string) bool {
	username = strings.TrimSpace(username)
	return len(username) >= 3 && len(username) <= 20 && !strings.ContainsAny(username, "<>&/\\\"'")
}

// Xabarlarni o'qish sikli
func (s *Server) readLoop(client *Client) {
	for {
		var msg Message
		err := websocket.JSON.Receive(client.conn, &msg)
		if err != nil {
			if err.Error() == "EOF" {
				log.Println("Client disconnected:", client.username)
				break
			}
			log.Println("Read error:", err)
			break
		}

		// Xabar validatsiyasi
		if !isValidMessage(msg) {
			continue
		}

		msg.Username = client.username
		msg.Time = time.Now().Format("15:04:05")
		msg.Timestamp = time.Now()

		// Xabar turini aniqlash va qayta ishlash
		switch msg.Type {
		case "message":
			if msg.Private && msg.To != "" {
				s.sendPrivateMessage(client, msg)
			} else {
				s.broadcastToRoom(msg, client.room)
			}
		case "join_room":
			s.handleJoinRoom(client, msg)
		case "leave_room":
			s.handleLeaveRoom(client)
		case "create_room":
			s.handleCreateRoom(client, msg)
		}

		// Xabarni tarixga saqlash
		if msg.Type == "message" && !msg.Private {
			s.saveToHistory(msg)
		}

		// Foydalanuvchi so'nggi faolligini yangilash
		client.lastSeen = time.Now()
	}
}

// Xabar validatsiyasi
func isValidMessage(msg Message) bool {
	return len(msg.Content) > 0 && len(msg.Content) <= 1000
}

// Xonaga qo'shilishni boshqarish
func (s *Server) handleJoinRoom(client *Client, msg Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	room, exists := s.rooms[msg.Room]
	if !exists {
		websocket.JSON.Send(client.conn, Message{
			Type:    "error",
			Content: "Room does not exist",
		})
		return
	}

	if room.isPrivate && room.owner != client.username {
		websocket.JSON.Send(client.conn, Message{
			Type:    "error",
			Content: "Cannot join private room",
		})
		return
	}

	if len(room.clients) >= room.maxClients {
		websocket.JSON.Send(client.conn, Message{
			Type:    "error",
			Content: "Room is full",
		})
		return
	}

	// Eski xonadan chiqish
	if client.room != "" {
		if oldRoom, exists := s.rooms[client.room]; exists {
			delete(oldRoom.clients, client.username)
		}
	}

	// Yangi xonaga kirish
	client.room = msg.Room
	room.clients[client.username] = client

	// Xona tarixini yuborish
	s.sendRoomHistory(client, msg.Room)

	// Xonaga kirish haqida xabar
	s.broadcastToRoom(Message{
		Type:      "system",
		Content:   fmt.Sprintf("%s joined the room", client.username),
		Room:      msg.Room,
		Time:      time.Now().Format("15:04:05"),
		Timestamp: time.Now(),
	}, msg.Room)
}

// Xonadan chiqishni boshqarish
func (s *Server) handleLeaveRoom(client *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if client.room == "" {
		return
	}

	if room, exists := s.rooms[client.room]; exists {
		delete(room.clients, client.username)
		s.broadcastToRoom(Message{
			Type:      "system",
			Content:   fmt.Sprintf("%s left the room", client.username),
			Room:      client.room,
			Time:      time.Now().Format("15:04:05"),
			Timestamp: time.Now(),
		}, client.room)
	}

	client.room = ""
}

// Yangi xona yaratishni boshqarish
func (s *Server) handleCreateRoom(client *Client, msg Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.rooms[msg.Room]; exists {
		websocket.JSON.Send(client.conn, Message{
			Type:    "error",
			Content: "Room already exists",
		})
		return
	}

	room := s.createRoom(msg.Room, client.username, msg.Private)
	client.room = msg.Room
	room.clients[client.username] = client

	websocket.JSON.Send(client.conn, Message{
		Type:    "system",
		Content: fmt.Sprintf("Room %s created successfully", msg.Room),
	})
}

// Shaxsiy xabar yuborish
func (s *Server) sendPrivateMessage(from *Client, msg Message) {
	s.mu.RLock()
	to, exists := s.clients[msg.To]
	s.mu.RUnlock()

	if !exists {
		websocket.JSON.Send(from.conn, Message{
			Type:    "error",
			Content: "User not found",
		})
		return
	}

	privateMsg := Message{
		Type:      "private",
		From:      from.username,
		To:        msg.To,
		Content:   msg.Content,
		Time:      time.Now().Format("15:04:05"),
		Timestamp: time.Now(),
	}

	websocket.JSON.Send(to.conn, privateMsg)
	websocket.JSON.Send(from.conn, privateMsg)
}

// Xonaga xabar yuborish
func (s *Server) broadcastToRoom(msg Message, roomName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	room, exists := s.rooms[roomName]
	if !exists {
		return
	}

	for _, client := range room.clients {
		websocket.JSON.Send(client.conn, msg)
	}
}

// Barcha mijozlarga xabar yuborish
func (s *Server) broadcast(msg Message) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, client := range s.conns {
		websocket.JSON.Send(client.conn, msg)
	}
}

// Xabarni tarixga saqlash
func (s *Server) saveToHistory(msg Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if history, exists := s.history[msg.Room]; exists {
		// Tarix uzunligini cheklash (masalan, so'nggi 100 ta xabar)
		if len(history) >= 100 {
			history = history[1:]
		}
		s.history[msg.Room] = append(history, msg)
	}
}

// Xona tarixini yuborish
func (s *Server) sendRoomHistory(client *Client, roomName string) {
	if history, exists := s.history[roomName]; exists {
		for _, msg := range history {
			websocket.JSON.Send(client.conn, msg)
		}
	}
}

// Ulanishni uzishni boshqarish
func (s *Server) handleDisconnect(client *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if client.room != "" {
		if room, exists := s.rooms[client.room]; exists {
			delete(room.clients, client.username)
		}
	}

	delete(s.conns, client.conn)
	delete(s.clients, client.username)

	s.broadcast(Message{
		Type:      "system",
		Content:   fmt.Sprintf("%s left the chat", client.username),
		Time:      time.Now().Format("15:04:05"),
		Timestamp: time.Now(),
	})
}

func main() {
	server := NewServer()

	// Static fayllar uchun handler
	fs := http.FileServer(http.Dir("static"))
	http.Handle("/", fs)

	// WebSocket handler
	http.Handle("/ws", websocket.Handler(server.handleWS))

	// Server holatini tekshirish uchun endpoint
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		s := struct {
			ActiveConnections int    `json:"active_connections"`
			ActiveRooms       int    `json:"active_rooms"`
			Uptime            string `json:"uptime"`
		}{
			ActiveConnections: len(server.conns),
			ActiveRooms:       len(server.rooms),
			Uptime:            time.Since(time.Now()).String(),
		}
		json.NewEncoder(w).Encode(s)
	})

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	fmt.Println("Server starting on :8090")
	fmt.Println("Visit http://localhost:8090/login.html to login")

	if err := http.ListenAndServe(":8090", nil); err != nil {
		log.Fatal("ListenAndServe:", err)
	}
}
