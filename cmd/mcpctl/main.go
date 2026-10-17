package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "usage: mcpctl <command>")
	os.Exit(2)
}
