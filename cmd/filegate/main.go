package main

import (
	"log"
	"os"

	"github.com/k2b-dev/filegate/v4/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}
