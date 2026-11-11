// Command cairn backs up MariaDB and PostgreSQL to S3-compatible storage.
package main

import (
	"os"

	"github.com/s-aga-r/cairn/internal/cli"
)

// exit is replaced in tests.
var exit = os.Exit

func main() {
	exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
