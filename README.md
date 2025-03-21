# GFWhell

A secure tunneling solution designed to mimic HTTPS traffic to maintain reliable internet connectivity in restricted network environments (cough, Great Firewall of China, cough).
## Overview

GFWhell implements a novel approach to network tunneling by:
- Encapsulating traffic within HTTPS using Server-Sent Events (SSE) for downstream and chunked POST requests for upstream
- Using standard compression (gzip) and encoding (base64) for data transfer
- Implementing robust error handling and automatic reconnection
- Providing seamless integration with SOCKS5 proxies

## Illustration
![image](https://github.com/user-attachments/assets/e860d663-9863-4e79-81a5-28086b7c0ba5)


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

## Skills Demonstrated

- Network Protocol Design
- Concurrent Programming
- Error Handling
