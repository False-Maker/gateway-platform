package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/elucid/gateway-platform/internal/control"
	"github.com/elucid/gateway-platform/internal/control/wrapper"
	"github.com/elucid/gateway-platform/internal/gateway"
)

func main() {
	role := flag.String("role", "", "runtime role: gateway, control, or wrapper")
	flag.Parse()

	var err error
	switch *role {
	case "gateway":
		err = gateway.Run(gateway.ConfigFromEnv())
	case "control":
		err = control.Run(control.ConfigFromEnv())
	case "wrapper":
		err = wrapper.Run(wrapper.ConfigFromEnv())
	default:
		args := flag.Args()
		if len(args) == 2 && args[0] == "wrapper" && args[1] == "run" {
			err = wrapper.Run(wrapper.ConfigFromEnv())
			break
		}
		flag.Usage()
		fmt.Fprintln(os.Stderr, "--role must be gateway, control, or wrapper (or use: wrapper run)")
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}
