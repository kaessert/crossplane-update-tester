package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "faketestserver: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:0", "address to listen on")
	examplesDir := flag.String("examples", "", "directory to walk for fixture manifests (required)")
	uppercaseFields := flag.String("uppercase-fields", "", "comma-separated spec.forProvider field names to upper-case when mirrored into status.atProvider")
	flag.Parse()

	if *examplesDir == "" {
		return fmt.Errorf("-examples is required")
	}

	uf := map[string]bool{}
	for _, f := range strings.Split(*uppercaseFields, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			uf[f] = true
		}
	}

	state := newServerState(uf)
	if err := state.seedFromDir(*examplesDir); err != nil {
		return fmt.Errorf("seeding from %s: %w", *examplesDir, err)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *listen, err)
	}

	srv := &http.Server{
		Handler:      &apiHandler{state: state},
		ReadTimeout:  serverReadTimeout,
		WriteTimeout: serverWriteTimeout,
	}

	// The exact contract hack/smoke-test.sh's server-startup helper reads:
	// one line, "LISTEN <host:port>", flushed before the server starts
	// accepting connections, so the caller never races against a port
	// that is not open yet.
	fmt.Printf("LISTEN %s\n", ln.Addr().String())
	_ = os.Stdout.Sync()

	log.SetOutput(os.Stderr)
	return srv.Serve(ln)
}
