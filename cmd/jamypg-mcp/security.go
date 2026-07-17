package main

import (
	"sqlon/internal/app"
)

// validateHTTPExposure prevents standalone mode from becoming remotely
// reachable merely because -addr was changed. The master admin token is not a
// substitute for this opt-in: in standalone mode it protects mutations and DB
// execution, but intentionally leaves read-only catalog MCP tools anonymous.
func validateHTTPExposure(transport, addr, metaDSN, adminToken string, publicMCP bool) error {
	return app.ValidateHTTPExposure(transport, addr, metaDSN, adminToken, publicMCP)
}
