package main

import (
	"context"
	"log"
	"os"

	"sqlon/internal/app"
)

func main() {
	if err := app.DefaultRuntime().Run(context.Background(), os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
