// Command jamypg-mcp is a one-release compatibility alias for sqlon.
// New deployments must use cmd/sqlon and the sqlon executable name.
package main

import (
	"context"
	"log"
	"os"

	"sqlon/internal/app"
)

func main() {
	log.Printf("DEPRECATED: jamypg-mcp has been renamed to sqlon; all behavior is provided by the SQLON runtime")
	if err := app.DefaultRuntime().Run(context.Background(), os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
