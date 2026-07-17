package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"sqlon/internal/catalog"
)

const maxStdioMessageBytes = 64 << 20

type StdioOptions struct {
	Logf func(format string, args ...any)
}

func ServeStdio(ctx context.Context, c *catalog.Catalog, in io.Reader, out io.Writer, opts StdioOptions) error {
	srv := NewServer(c, Options{Stateful: false})
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStdioMessageBytes)
	writer := bufio.NewWriter(out)
	defer writer.Flush()

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			if err := writeStdioResponse(writer, errorResponse(nil, -32700, "parse error", err.Error())); err != nil {
				return err
			}
			continue
		}
		if msg.JSONRPC != "" && msg.JSONRPC != "2.0" {
			if msg.ID != nil {
				if err := writeStdioResponse(writer, errorResponse(msg.ID, -32600, "invalid request", "jsonrpc must be 2.0")); err != nil {
					return err
				}
			}
			continue
		}
		if msg.Method == "" {
			if msg.ID != nil {
				if err := writeStdioResponse(writer, errorResponse(msg.ID, -32600, "invalid request", "method is required")); err != nil {
					return err
				}
			}
			continue
		}
		if msg.ID == nil {
			if opts.Logf != nil && msg.Method == "notifications/cancelled" {
				opts.Logf("received cancellation notification")
			}
			continue
		}
		resp := srv.handleRequest(ctx, msg)
		if err := writeStdioResponse(writer, resp); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stdio: %w", err)
	}
	return nil
}

func writeStdioResponse(w *bufio.Writer, resp rpcResponse) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}
