package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ======================================================
// Configuration
// ======================================================
const (
	// Network configuration
	LocalListenAddr = ":30001"
	ShadowSocksAddr = "127.0.0.1:5080"

	// Timeouts and intervals
	readTimeout          = 180 * time.Second      // Read timeout from the local connection
	sseWriteTimeout      = 10 * time.Second       // Write timeout for SSE responses
	sessionCleanupDelay  = 100 * time.Millisecond // Delay before closing session
	sseHeartbeatInterval = 20 * time.Second       // Interval for SSE heartbeat messages

	// Buffer and chunk sizes
	bufferSize       = 128 * 1024 // Size of the local read buffer (128 KB)
	sseChunkSize     = 4 * 1024   // Maximum chunk size for SSE events (4 KB)
	sseChannelBuffer = 100        // Size of the global SSE channel buffer

	// TLS configuration
	tlsCertFile   = "server.crt"
	tlsKeyFile    = "server.key"
	tlsMinVersion = tls.VersionTLS12
)

// postPayload is the JSON payload sent from client POST requests.
type postPayload struct {
	// Data holds gzipped+base64 encoded data.
	Data string `json:"data,omitempty"`
	// Final signals the final POST chunk.
	Final bool `json:"final,omitempty"`
}

// ssePayload is the JSON structure sent to the client in SSE.
type ssePayload struct {
	RequestID  string `json:"request_id"`
	Data       string `json:"data"`
	Part       int    `json:"part,omitempty"`        // New: current part number (1-indexed)
	TotalParts int    `json:"total_parts,omitempty"` // New: total number of parts
}

// ======================================================
// Global SSE channel and connection control
// ======================================================
var (
	globalSSEChan      = make(chan string, sseChannelBuffer)
	globalSSEMu        sync.Mutex
	globalSSEConnected bool
)

// ======================================================
// Session and Shadowsocks connection management
// ======================================================

// Session bridges data between the client (via POST) and Shadowsocks.
type Session struct {
	RequestID     string
	Conn          net.Conn
	writeMutex    sync.Mutex
	cancel        context.CancelFunc
	closed        bool
	mu            sync.Mutex
	collectedData string // New field for accumulating base64 encoded chunks
}

// Global sessions indexed by request id.
var (
	sessionMu sync.RWMutex
	sessions  = make(map[string]*Session)
)

// getSession retrieves a session by request ID.
func getSession(requestID string) *Session {
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return sessions[requestID]
}

// addSession registers a new session.
func addSession(s *Session) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	sessions[s.RequestID] = s
}

// removeSession removes a session.
func removeSession(requestID string) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	delete(sessions, requestID)
}

// gzipAndBase64 compresses input data then base64–encodes it.
func gzipAndBase64(input []byte) (string, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write(input)
	if err != nil {
		return "", err
	}
	if err = gw.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// base64AndGunzip performs the reverse operation.
func base64AndGunzip(encoded string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	gr, err := gzip.NewReader(bytes.NewReader(decoded))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	return io.ReadAll(gr)
}

// closeSession closes and cleans up a session.
func (s *Session) closeSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	time.Sleep(sessionCleanupDelay)
	if s.Conn != nil {
		s.Conn.Close()
	}
	removeSession(s.RequestID)
	log.Printf("Session %s closed", s.RequestID)
}

// startSession reads from the Shadowsocks connection and pushes data to the global SSE channel.
func (s *Session) startSession(ctx context.Context) {
	go func() {
		defer s.closeSession() // Clean up on exit.
		buf := make([]byte, bufferSize)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// Set a read deadline.
			s.Conn.SetReadDeadline(time.Now().Add(readTimeout))
			n, err := s.Conn.Read(buf)
			if err != nil {
				if !errors.Is(err, io.EOF) &&
					!strings.Contains(err.Error(), "use of closed network connection") {
					log.Printf("Error reading from Shadowsocks (session %s): %v", s.RequestID, err)
				} else {
					log.Printf("Shadowsocks connection closed (session %s)", s.RequestID)
				}
				return
			}
			if n <= 0 {
				continue
			}
			// Compress and encode the read data.
			encodedStr, err := gzipAndBase64(buf[:n])
			if err != nil {
				log.Printf("Error encoding data for session %s: %v", s.RequestID, err)
				continue
			}

			// Calculate total number of chunks based on sseChunkSize.
			totalLen := len(encodedStr)
			totalChunks := int(math.Ceil(float64(totalLen) / float64(sseChunkSize)))

			// Send each chunk as a separate SSE message.
			for i := 0; i < totalChunks; i++ {
				start := i * sseChunkSize
				end := start + sseChunkSize
				if end > totalLen {
					end = totalLen
				}
				chunkData := encodedStr[start:end]
				sseObj := ssePayload{
					RequestID:  s.RequestID,
					Data:       chunkData,
					Part:       i + 1, // 1-indexed chunk number
					TotalParts: totalChunks,
				}
				jsonData, err := json.Marshal(sseObj)
				if err != nil {
					log.Printf("Error marshaling SSE payload for session %s: %v", s.RequestID, err)
					continue
				}
				// Send JSON string on the global SSE channel.
				select {
				case globalSSEChan <- string(jsonData):
				case <-ctx.Done():
					return
				default:
					log.Printf("Warning: SSE channel full, dropping message for session %s", s.RequestID)
				}
			}
		}
	}()

}

// ======================================================
// HTTP Handlers
// ======================================================
// uploadHandler handles POST /upload requests and collects all chunks before decoding.
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract query parameters.
	q := r.URL.Query()
	requestID := q.Get("requestid")
	partStr := q.Get("part")
	totalPartStr := q.Get("totalpart")
	part, err := strconv.Atoi(partStr)
	if err != nil {
		http.Error(w, "Invalid part parameter", http.StatusBadRequest)
		return
	}
	totalPart, err := strconv.Atoi(totalPartStr)
	if err != nil {
		http.Error(w, "Invalid totalpart parameter", http.StatusBadRequest)
		return
	}
	// log.Printf("Received upload: request_id=%s, part=%d, totalPart=%d", requestID, part, totalPart)

	// Decode the JSON body.
	var payload postPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Retrieve or create the session.
	sess := getSession(requestID)
	if sess == nil {
		conn, err := net.Dial("tcp", ShadowSocksAddr)
		if err != nil {
			http.Error(w, "Failed to connect to Shadowsocks server", http.StatusInternalServerError)
			log.Printf("Failed to connect to Shadowsocks for session %s: %v", requestID, err)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		sess = &Session{
			RequestID:     requestID,
			Conn:          conn,
			cancel:        cancel,
			collectedData: "", // Initialize accumulator.
		}
		addSession(sess)
		// Optionally, start any session-related goroutines.
		go sess.startSession(ctx)
		log.Printf("Created new session for request_id=%s", requestID)
	}

	// If the final flag is set, close the session.
	if payload.Final {
		go sess.closeSession()
		log.Printf("Final chunk received for session %s, session closed", requestID)
	}

	// Append any provided data chunk to the session's accumulator.
	if payload.Data != "" {
		sess.mu.Lock()
		sess.collectedData += payload.Data
		sess.mu.Unlock()
		// log.Printf("Accumulated chunk for session %s: received %d bytes", requestID, len(payload.Data))
	} else {
		log.Printf("Received heartbeat/empty chunk for session %s", requestID)
	}

	if part == totalPart {
		// Retrieve all collected data.
		sess.mu.Lock()
		dataStr := sess.collectedData
		sess.mu.Unlock()

		if dataStr != "" {
			// Decode and decompress the entire collected data.
			data, err := base64AndGunzip(dataStr)
			if err != nil {
				http.Error(w, fmt.Sprintf("Data decode error: %v", err), http.StatusBadRequest)
				log.Printf("Decode error for session %s: %v", requestID, err)
				return
			}

			// Write the decompressed data to the Shadowsocks connection.
			sess.writeMutex.Lock()
			_, err = sess.Conn.Write(data)
			sess.writeMutex.Unlock()
			if err != nil {
				http.Error(w, fmt.Sprintf("Error writing to Shadowsocks: %v", err), http.StatusInternalServerError)
				log.Printf("Error writing to Shadowsocks for session %s: %v", requestID, err)
				return
			}
			// log.Printf("Forwarded decompressed data for session %s, total bytes: %d", requestID, len(data))
			sess.mu.Lock()
			sess.collectedData = ""
			sess.mu.Unlock()
		}

	}

	// Always return a 200 OK response.
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// sseHandler handles GET /sse requests and streams the global SSE channel.
func sseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Allow only one global SSE connection.
	globalSSEMu.Lock()
	if globalSSEConnected {
		globalSSEMu.Unlock()
		http.Error(w, "Global SSE connection already established", http.StatusConflict)
		return
	}
	globalSSEConnected = true
	globalSSEMu.Unlock()
	defer func() {
		globalSSEMu.Lock()
		globalSSEConnected = false
		globalSSEMu.Unlock()
		log.Printf("Global SSE connection closed")
	}()

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Send an initial message.
	_, err := w.Write([]byte("data: connected\n\n"))
	if err != nil {
		log.Printf("Error sending initial SSE message: %v", err)
		return
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			log.Printf("SSE connection closed by client")
			// clear all messages in the channel
			for len(globalSSEChan) > 0 {
				<-globalSSEChan
				// clear all sessions and close them

			}
			return
		case msg := <-globalSSEChan:
			// Write the message.
			sseMsg := fmt.Sprintf("data: %s\n\n", strings.TrimSpace(msg))
			// Set a write deadline.
			if f, ok := w.(interface{ SetWriteDeadline(time.Time) error }); ok {
				_ = f.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			}
			_, err := w.Write([]byte(sseMsg))
			if err != nil {
				log.Printf("Error writing SSE message: %v", err)
				return
			}
			flusher.Flush()
		// Handle a heartbeat message.
		case <-time.After(sseHeartbeatInterval):
			_, err := w.Write([]byte("data: heartbeat\n\n"))
			if err != nil {
				log.Printf("Error writing SSE heartbeat: %v", err)
				return
			}
			flusher.Flush()

		}
	}
}

// healthHandler provides a simple health check.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("OK"))
}

// ======================================================
// main() for the server
// ======================================================
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/upload", uploadHandler)
	mux.HandleFunc("/sse", sseHandler)
	mux.HandleFunc("/health", healthHandler)

	tlsConfig := &tls.Config{
		MinVersion:         tlsMinVersion,
		InsecureSkipVerify: true,
	}

	// Create a server without timeouts for SSE
	sseServer := &http.Server{
		Addr: LocalListenAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/sse" {
				sseHandler(w, r)
			} else {
				mux.ServeHTTP(w, r)
			}
		}),
		TLSConfig: tlsConfig,
		// No timeouts for SSE connections
	}

	log.Printf("Server listening on https://%s", LocalListenAddr)
	if err := sseServer.ListenAndServeTLS(tlsCertFile, tlsKeyFile); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
