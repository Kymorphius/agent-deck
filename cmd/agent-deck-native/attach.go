package main

// matrix/hardened: detach closes only client pipes; the independent host continues owning ConPTY.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"golang.org/x/term"
)

type inputChunk struct {
	data []byte
	err  error
}

func runAttach(config options, input, output *os.File, stderr io.Writer) error {
	inputFD, outputFD := int(input.Fd()), int(output.Fd())
	if !term.IsTerminal(inputFD) || !term.IsTerminal(outputFD) {
		return errors.New("attach requires an interactive console; use send/capture for redirected I/O")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	cols, rows, err := term.GetSize(outputFD)
	if err != nil {
		return fmt.Errorf("read console dimensions: %w", err)
	}
	if _, err = callHost(ctx, config.Pipe, terminalhost.Request{Op: "resize", Cols: cols, Rows: rows}); err != nil {
		return err
	}
	snapshot, err := callHost(ctx, config.Pipe, terminalhost.Request{Op: "capture"})
	if err != nil {
		return err
	}
	raw, err := term.MakeRaw(inputFD)
	if err != nil {
		return fmt.Errorf("enable raw console input: %w", err)
	}
	defer term.Restore(inputFD, raw)
	restoreOutput, err := enableConsoleOutput(outputFD)
	if err != nil {
		return err
	}
	defer restoreOutput()
	// matrix/hardened: client-only cleanup restores shell display modes after detach or failure.
	defer fmt.Fprint(output, "\x1b[0m\x1b[?25h\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1049l\r\n")
	if err := paintSnapshot(output, snapshot); err != nil {
		return err
	}
	after := snapshot.HeadOffset
	lastRender, lastX, lastY := snapshot.Render, snapshot.CursorX, snapshot.CursorY
	inputChunks := make(chan inputChunk, 8)
	go readInput(ctx, input, inputChunks)
	poll := time.NewTicker(40 * time.Millisecond)
	defer poll.Stop()
	resize := time.NewTicker(250 * time.Millisecond)
	defer resize.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case chunk := <-inputChunks:
			forward, detach := splitDetach(chunk.data)
			if len(forward) > 0 {
				if _, err := callHost(ctx, config.Pipe, terminalhost.Request{Op: "send", Data: forward}); err != nil {
					return err
				}
			}
			if detach {
				fmt.Fprint(stderr, "\r\nDetached; host and child continue running.\r\n")
				return nil
			}
			if chunk.err != nil {
				if errors.Is(chunk.err, io.EOF) {
					return nil
				}
				return fmt.Errorf("read console input: %w", chunk.err)
			}
		case <-resize.C:
			newCols, newRows, sizeErr := term.GetSize(outputFD)
			if sizeErr != nil {
				return fmt.Errorf("read console dimensions: %w", sizeErr)
			}
			if newCols != cols || newRows != rows {
				if _, err := callHost(ctx, config.Pipe, terminalhost.Request{Op: "resize", Cols: newCols, Rows: newRows}); err != nil {
					return err
				}
				cols, rows = newCols, newRows
			}
		case <-poll.C:
			response, err := callHost(ctx, config.Pipe, terminalhost.Request{Op: "capture", After: after})
			if err != nil {
				return err
			}
			// matrix/hardened: the host emulator is the sole query responder.
			// Render snapshots instead of forwarding raw DA/DSR/OSC requests to
			// a second terminal, which would send duplicate replies to the child.
			if response.Render != lastRender || response.CursorX != lastX || response.CursorY != lastY {
				if err := paintSnapshot(output, response); err != nil {
					return err
				}
				lastRender, lastX, lastY = response.Render, response.CursorX, response.CursorY
			}
			after = response.HeadOffset
			if !response.Running && after >= response.HeadOffset {
				return nil
			}
		}
	}
}

func paintSnapshot(output io.Writer, response terminalhost.Response) error {
	// matrix/hardened: screen newlines need explicit CR in a raw Windows VT output console.
	render := strings.ReplaceAll(response.Render, "\n", "\r\n")
	_, err := fmt.Fprintf(output, "\x1b[0m\x1b[?25l\x1b[2J\x1b[H%s\x1b[%d;%dH\x1b[?25h", render, response.CursorY+1, response.CursorX+1)
	return err
}

func readInput(ctx context.Context, input io.Reader, chunks chan<- inputChunk) {
	buffer := make([]byte, 4096)
	for {
		count, err := input.Read(buffer)
		chunk := inputChunk{data: append([]byte(nil), buffer[:count]...), err: err}
		if count > 0 || err != nil {
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func splitDetach(data []byte) (forward []byte, detach bool) {
	for index, char := range data {
		if char == 0x11 {
			return data[:index], true
		}
	}
	return data, false
}
