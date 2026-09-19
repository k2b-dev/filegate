package main

import (
	"log"
	"os"

	"github.com/k2b-dev/filegate/v6/cli"
	"github.com/k2b-dev/filegate/v6/infra/filesystem"
)

func main() {
	if handled, err := filesystem.RunExecutionWorker(); handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if err := cli.Execute(); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}
