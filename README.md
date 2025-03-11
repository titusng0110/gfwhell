# GFWhell

A secure tunneling solution designed to maintain reliable internet connectivity in restricted network environments (cough, Great Firewall of China, cough). Built with Go, this project demonstrates advanced networking concepts and modern software engineering practices.

## Overview

GFWhell implements a novel approach to network tunneling by:
- Encapsulating traffic within HTTPS using Server-Sent Events (SSE) for downstream and chunked POST requests for upstream
- Using standard compression (gzip) and encoding (base64) for data transfer
- Implementing robust error handling and automatic reconnection
- Providing seamless integration with SOCKS5 proxies

## Technical Highlights

- **Advanced Protocol Design**: Bidirectional communication through HTTP(S) using SSE and POST requests
- **Data Handling**: 
  - Chunked transfer encoding for large payloads
  - Buffered I/O for efficient memory usage
  - Session-based data management
- **Reliability Features**: 
  - Automatic retry mechanism
  - Connection keepalive and heartbeat monitoring
  - Graceful session management
- **Security Considerations**: TLS encryption, configurable timeout parameters

## Architecture

The system consists of two main components:
- **Client**: Listens for local SOCKS5 connections and tunnels traffic to the server
- **Server**: Handles incoming tunneled connections and forwards traffic to the destination

## Skills Demonstrated

- Network Protocol Design
- Concurrent Programming
- Error Handling
