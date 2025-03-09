package main

import (
	"bufio"
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
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ======================================================
// Configuration
// ======================================================
const (
	// Listen for local Socks5 connections.
	LocalListenAddr = ":30000"
	// Remote server URL (your censorship bypass server).
	RemoteServerURL = "https://127.0.0.1:30001"
	// Maximum size for each POST chunk (4KB).
	chunkSize = 4 * 1024
	// Read timeout from the local connection.
	readTimeout = 30 * time.Second
	// HTTP client timeout.
	httpTimeout = 60 * time.Second
	// Maximum retries for POST requests.
	maxRetries     = 3
	retryBaseDelay = 500 * time.Millisecond
	// Size of the local read buffer.
	bufferSize = 32 * 1024
)

// ssePayload is the JSON structure already gzipped and base64–encoded
// that is sent via SSE from the server.
type ssePayload struct {
	RequestID string `json:"request_id"`
	Data      string `json:"data"`
}

// postPayload is the JSON payload sent via POST from client to server.
type postPayload struct {
	// Data holds a chunk (gzipped+base64 encoded).
	Data string `json:"data,omitempty"`
	// Final signals that this is the final chunk for a session.
	Final bool `json:"final,omitempty"`
}

// session represents a Socks5 connection session.
type session struct {
	requestID string
	conn      net.Conn
	// protect concurrent writes to the connection.
	writeMutex sync.Mutex
	// Last activity timestamp
	lastActivity time.Time
}

// Global map to store sessions by request id.
var (
	sessionMu sync.RWMutex
	sessions  = make(map[string]*session)
)

// =================================================
// Utility functions
// =================================================

// generateRequestID creates a new request identifier.
func generateRequestID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Intn(10000))
}

// gzipAndBase64 compresses data with gzip and then base64–encodes it.
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

// base64AndGunzip decodes from base64 and then decompresses with gzip.
func base64AndGunzip(input string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(input)
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

// isTemporaryError returns true if err is likely temporary.
func isTemporaryError(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Timeout() || urlErr.Temporary()
	}
	return false
}

// =================================================
// HTTP POST functions to send chunks
// =================================================

// sendChunk sends one POST request with the given JSON payload.
func sendChunk(ctx context.Context, client *http.Client, reqID string, part, totalPart int, payload postPayload) error {
	targetURL := fmt.Sprintf("%s/upload?requestid=%s&part=%d&totalpart=%d", RemoteServerURL, reqID, part, totalPart)
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Use a random User-Agent header.
	uas := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
		"Mozilla/5.0 (X11; Linux x86_64)",
	}
	req.Header.Set("User-Agent", uas[rand.Intn(len(uas))])
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("non-200 response: %d, body: %s", resp.StatusCode, string(body))
	}
	return nil
}

// sendChunkWithRetry attempts to send a chunk with exponential backoff.
func sendChunkWithRetry(ctx context.Context, client *http.Client, reqID string, part, totalPart int, payload postPayload) error {
	var lastErr error
	backoff := retryBaseDelay

	for attempt := 0; attempt <= maxRetries; attempt++ {
		err := sendChunk(ctx, client, reqID, part, totalPart, payload)
		if err == nil {
			return nil
		}
		lastErr = err
		log.Printf("POST send error (attempt %d/%d): %v", attempt+1, maxRetries+1, err)

		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt < maxRetries && isTemporaryError(err) {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff *= 2
			continue
		}
		return err
	}
	return fmt.Errorf("max retries exceeded: %w", lastErr)
}

// =================================================
// Global SSE receiver (Only one SSE connection per client)
// =================================================

var (
	sseClient *http.Client
)

// runSSEReceiver establishes the global SSE connection with the server and
// dispatches incoming data to the corresponding sessions.
func runSSEReceiver() {
	// Create an HTTP client with a reasonable timeout.
	sseClient = &http.Client{
		// Timeout: httpTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true,
			},
		},
	}

	for {
		err := subscribeSSE() // blocks until error or context cancellation
		if err != nil {
			log.Printf("SSE subscribe error: %v. Reconnecting in 5 seconds...", err)
			time.Sleep(5 * time.Second)
		}
	}
}

// subscribeSSE connects to the /sse endpoint and dispatches incoming events.
func subscribeSSE() error {
	url := fmt.Sprintf("%s/sse", RemoteServerURL)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("SSE new request error: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")

	resp, err := sseClient.Do(req)
	if err != nil {
		return fmt.Errorf("SSE connection error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SSE non-200 status: %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// Expect lines in the format “data: <payload>”
		const prefix = "data:"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		dataStr := strings.TrimSpace(line[len(prefix):])
		// Skip heartbeat messages (or initial handshake).
		if dataStr == "heartbeat" || dataStr == "connected" {
			continue
		}
		// Decode the JSON payload from the server.
		var sseMsg ssePayload
		if err := json.Unmarshal([]byte(dataStr), &sseMsg); err != nil {
			log.Printf("SSE JSON unmarshal error: %v", err)
			continue
		}
		// Get the target session.
		sessionMu.RLock()
		sess, ok := sessions[sseMsg.RequestID]
		sessionMu.RUnlock()
		if !ok {
			// No session registered for this request id.
			log.Printf("No session for request_id=%s", sseMsg.RequestID)
			continue
		}
		// Decode the data: first base64 then gunzip.
		decoded, err := base64AndGunzip(sseMsg.Data)
		if err != nil {
			log.Printf("SSE decode error for request_id=%s: %v", sseMsg.RequestID, err)
			continue
		}
		// Write the decoded data to the local connection.
		sess.writeMutex.Lock()
		_, err = sess.conn.Write(decoded)
		sess.writeMutex.Unlock()
		if err != nil {
			log.Printf("Error writing to session %s: %v", sseMsg.RequestID, err)
			// Optionally close and remove the session.
			sessionMu.Lock()
			delete(sessions, sseMsg.RequestID)
			sessionMu.Unlock()
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("SSE scanner error: %w", err)
	}
	return nil
}

// =================================================
// Session handling (for each Socks5 connection)
// =================================================

// handleConnection processes a new Socks5 connection.
func handleConnection(conn net.Conn, client *http.Client) {
	defer conn.Close()

	reqID := generateRequestID()
	sess := &session{
		requestID:    reqID,
		conn:         conn,
		lastActivity: time.Now(),
	}
	// Register the session.
	sessionMu.Lock()
	sessions[reqID] = sess
	sessionMu.Unlock()

	log.Printf("New session started, request_id=%s", reqID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Function to read data from the local connection, chunk it and send it.
	go func() {
		buffer := make([]byte, bufferSize)
		part := 0
		for {
			conn.SetReadDeadline(time.Now().Add(readTimeout))
			n, err := conn.Read(buffer)
			if n > 0 {
				sess.lastActivity = time.Now()
				// Compress and encode data.
				encodedStr, err := gzipAndBase64(buffer[:n])
				if err != nil {
					log.Printf("gzip/base64 error on session %s: %v", reqID, err)
					return
				}
				// Calculate chunks.
				totalLen := len(encodedStr)
				totalChunks := int(math.Ceil(float64(totalLen) / float64(chunkSize)))
				// When there is data, send each chunk.
				for i := 0; i < totalChunks; i++ {
					part++
					payload := postPayload{}
					if totalLen > 0 {
						start := i * chunkSize
						end := start + chunkSize
						if end > totalLen {
							end = totalLen
						}
						payload.Data = encodedStr[start:end]
					}
					// final flag is false here.
					err = sendChunkWithRetry(ctx, client, reqID, part, 0, payload)
					if err != nil {
						log.Printf("Error sending data chunk for session %s: %v", reqID, err)
						return
					}
				}
			}
			if err != nil {
				// On timeout, continue waiting.
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				// On EOF or other errors, break the loop.
				log.Printf("Local connection read error for session %s: %v", reqID, err)
				break
			}
		}
		// Send the final (empty) chunk to signal session end.
		payload := postPayload{Final: true}
		part++
		_ = sendChunkWithRetry(ctx, client, reqID, part, part, payload)
		// Remove the session.
		sessionMu.Lock()
		delete(sessions, reqID)
		sessionMu.Unlock()
		log.Printf("Session %s ended", reqID)
	}()

	// Block until connection error.
	<-ctx.Done()
}

// =================================================
// main() for the client
// =================================================
func main() {

	// Create an HTTP client for POST requests.
	httpClient := &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true,
			},
		},
	}

	// Start the global SSE receiver.
	go runSSEReceiver()

	// Listen for local connections (Socks5 front-end).
	ln, err := net.Listen("tcp", LocalListenAddr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", LocalListenAddr, err)
	}
	defer ln.Close()
	log.Printf("Client listening on %s", LocalListenAddr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}
		go handleConnection(conn, httpClient)
	}
}
