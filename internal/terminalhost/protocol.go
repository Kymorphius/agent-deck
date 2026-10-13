// Package terminalhost is the experimental durable native terminal host.
// matrix/hardened: this protocol is independent of the production tmux manager.
package terminalhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	MaxInputBytes    = 64 * 1024
	MaxOutputBytes   = 256 * 1024
	HistoryBytes     = 4 * 1024 * 1024
	maxRequestBytes  = 128 * 1024
	maxResponseBytes = 8 * 1024 * 1024
)

// Process owns the terminal independently of any connected client.
type Process interface {
	PID() uint32
	Input() io.Writer
	Output() io.Reader
	Resize(cols, rows int) error
	Wait() (uint32, error)
	Close() error
}

type Request struct {
	Op    string `json:"op"`
	Data  []byte `json:"data,omitempty"`
	After uint64 `json:"after,omitempty"`
	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
}

type Response struct {
	OK          bool    `json:"ok"`
	Error       string  `json:"error,omitempty"`
	PID         uint32  `json:"pid"`
	HostPID     int     `json:"host_pid"`
	Running     bool    `json:"running"`
	ExitCode    *uint32 `json:"exit_code,omitempty"`
	Cols        int     `json:"cols"`
	Rows        int     `json:"rows"`
	StartOffset uint64  `json:"start_offset"`
	EndOffset   uint64  `json:"end_offset"`
	HeadOffset  uint64  `json:"head_offset"`
	Truncated   bool    `json:"truncated,omitempty"`
	Output      []byte  `json:"output,omitempty"`
	Screen      string  `json:"screen,omitempty"`
	Render      string  `json:"render,omitempty"`
	CursorX     int     `json:"cursor_x"`
	CursorY     int     `json:"cursor_y"`
}

// Call performs one request; the caller retains ownership of the connection.
// Cancellation closes this one client connection, never the hosted process.
func Call(ctx context.Context, conn net.Conn, request Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return Response{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if len(request.Data) > MaxInputBytes {
		return Response{}, fmt.Errorf("input exceeds %d bytes", MaxInputBytes)
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return Response{}, err
	}
	var response Response
	if err := json.NewDecoder(io.LimitReader(conn, maxResponseBytes)).Decode(&response); err != nil {
		return Response{}, err
	}
	if !response.OK {
		return response, fmt.Errorf("native terminal host: %s", response.Error)
	}
	return response, nil
}
