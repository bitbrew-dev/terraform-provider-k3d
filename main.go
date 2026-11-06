// Command terraform-provider-k3d serves the k3d Terraform provider.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

const providerAddress = "registry.terraform.io/bitbrew-dev/k3d"

var version = "dev"

func main() {
	var debug bool

	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers such as delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: providerAddress,
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err)
	}
}
