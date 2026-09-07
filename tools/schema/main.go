package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hoorific/internal/admin"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("output", "", "OpenAPI output path")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "--output is required")
		os.Exit(2)
	}
	b, e := json.MarshalIndent(admin.OpenAPISpec(), "", "  ")
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if e = os.MkdirAll(filepath.Dir(*out), 0700); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if e = os.WriteFile(*out, append(b, '\n'), 0600); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
