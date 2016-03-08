// apiserver is the main api server.
package main

import (
	"fmt"
	"github.com/gosharplite/sketch/cmd/skt-apiserver/app"
	"github.com/gosharplite/sketch/cmd/skt-apiserver/app/options"
	"math/rand"
	"os"
	"runtime"
	"time"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	rand.Seed(time.Now().UTC().UnixNano())

	s := options.NewAPIServer()

	if err := app.Run(s); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
