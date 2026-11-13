// Command local serves the spend guard on this machine with the Functions
// Framework, for manual tests. It is not deployed.
//
//	FUNCTION_TARGET=ScaleToZero DRY_RUN=true PROJECT_ID=... go run ./cmd/local
package main

import (
	"flag"
	"log"
	"net"

	"github.com/GoogleCloudPlatform/functions-framework-go/funcframework"
	_ "github.com/codewithmuh/mamori-sre-gke/tools/spend-guard" // registers ScaleToZero
)

func main() {
	// Loopback by default: this is a test server, not a service. Inside a
	// container, pass -host 0.0.0.0 and publish the port on 127.0.0.1 only.
	host := flag.String("host", "127.0.0.1", "address to listen on")
	port := flag.String("port", "8080", "port to listen on")
	flag.Parse()

	log.Printf("serving on %s", net.JoinHostPort(*host, *port))
	if err := funcframework.StartHostPort(*host, *port); err != nil {
		log.Fatalf("funcframework: %v", err)
	}
}
