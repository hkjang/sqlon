package main

import (
	"strings"
	"testing"
)

func TestValidateHTTPExposure(t *testing.T) {
	tests := []struct {
		name       string
		transport  string
		addr       string
		metaDSN    string
		adminToken string
		publicMCP  bool
		wantErr    string
	}{
		{name: "default IPv4 loopback", transport: "http", addr: "127.0.0.1:6767"},
		{name: "IPv4 loopback range", transport: "http", addr: "127.0.0.2:6767"},
		{name: "IPv6 loopback", transport: "streamable-http", addr: "[::1]:6767"},
		{name: "localhost", transport: "HTTP", addr: "localhost:6767"},
		{name: "stdio ignores HTTP address", transport: "stdio", addr: "0.0.0.0:6767"},
		{name: "authenticated public bind", transport: "http", addr: "0.0.0.0:6767", metaDSN: "postgres://meta"},
		{name: "explicit public wildcard bind", transport: "http", addr: "0.0.0.0:6767", adminToken: "secret", publicMCP: true},
		{name: "explicit public interface bind", transport: "http", addr: "192.0.2.10:6767", adminToken: "secret", publicMCP: true},
		{name: "public opt-in still needs admin token", transport: "http", addr: "0.0.0.0:6767", publicMCP: true, wantErr: "without an admin token"},
		{name: "admin token alone is not public opt-in", transport: "http", addr: "0.0.0.0:6767", adminToken: "secret", wantErr: "refusing standalone HTTP"},
		{name: "IPv4 wildcard denied", transport: "http", addr: "0.0.0.0:6767", wantErr: "refusing standalone HTTP"},
		{name: "IPv6 wildcard denied", transport: "http", addr: "[::]:6767", wantErr: "refusing standalone HTTP"},
		{name: "empty wildcard denied", transport: "http", addr: ":6767", wantErr: "refusing standalone HTTP"},
		{name: "interface address denied", transport: "http", addr: "192.0.2.10:6767", wantErr: "refusing standalone HTTP"},
		{name: "hostname denied", transport: "http", addr: "db-gateway.internal:6767", wantErr: "refusing standalone HTTP"},
		{name: "malformed address", transport: "http", addr: "127.0.0.1", wantErr: "invalid HTTP listen address"},
		{name: "unsupported transport", transport: "sse", addr: "127.0.0.1:6767", wantErr: "unsupported transport"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHTTPExposure(tt.transport, tt.addr, tt.metaDSN, tt.adminToken, tt.publicMCP)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
