package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/RiRa12621/machine-api-provider-stackit/pkg/actuator"
	"github.com/RiRa12621/machine-api-provider-stackit/pkg/cloud"
	providermanager "github.com/RiRa12621/machine-api-provider-stackit/pkg/manager"
	machinecontroller "github.com/openshift/machine-api-operator/pkg/controller/machine"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	crmanager "sigs.k8s.io/controller-runtime/pkg/manager"
)

var version = "development"

func main() {
	options := providermanager.DefaultOptions()
	options.BindFlags(flag.CommandLine)
	printVersion := flag.Bool("version", false, "Print the provider version and exit")
	flag.Parse()
	if *printVersion {
		fmt.Println(version)
		return
	}
	ctrl.SetLogger(klog.NewKlogr())
	err := providermanager.Run(ctrl.SetupSignalHandler(), options, func(mgr crmanager.Manager) (machinecontroller.Actuator, error) {
		return actuator.New(mgr.GetClient(), mgr.GetAPIReader(), cloud.NewClient), nil
	})
	if err != nil {
		klog.Error(err)
		os.Exit(1)
	}
}
