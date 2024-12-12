package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
)

var (
	name = "kgrok"
	port = 28688

	path      string
	namespace string
	dryRun    bool
	decoder   = yaml.NewDecodingSerializer(
		unstructured.UnstructuredJSONScheme,
	)
	command = &cobra.Command{
		Use: name + " [flags] [LOCAL_PORT:]REMOTE_PORT [...[LOCAL_PORT_N:]REMOTE_PORT_N]",
		Long: "This command adds a new service and deployment to the cluster (or applies any template provided through stdin), " +
			"and establishes a tunnel from localhost to one or more remote ports.\n\n" +
			"The LOCAL_PORT:REMOTE_PORT format follows the same conventions as kubectl port-forward. " +
			"For more details, please refer to the official documentation.",
		Example: `
# using a template to create an HTTPRoute, a service and a deployment tunneling from port 8080
cat <<EOF | ` + name + ` 8080
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: foo-staging
spec:
  hostnames:
    - foo.staging.example.com
  parentRefs:
    - name: contour
      namespace: contour
      sectionName: example-staging
  rules:
    - backendRefs:
        - kind: Service
          name: {{.Name}}
          port: 8081` + string(tplt) + "EOF",
		Args: cobra.MinimumNArgs(1), // we want at least one port mapping
		RunE: func(cmd *cobra.Command, args []string) error {
			// https://github.com/spf13/cobra/issues/340
			cmd.SilenceUsage = true
			return run(args)
		},
	}
)

func init() {
	flags := command.PersistentFlags()
	flags.StringVar(&path, "template", "", "File to read template from")
	flags.BoolVar(&dryRun, "dry-run", false,
		"Dry run mode, only render template, do not apply or tunnel anything")
	flags.StringVarP(&namespace, "namespace", "n", "",
		"Namespace to deploy all the templates to (override the one from config)")
}

func main() {
	if err := command.Execute(); err != nil {
		os.Exit(1)
	}
}

func interrupt() context.Context {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)
		<-sigint
		fmt.Printf("\nreceived interrupt, aborting...\n")
		cancel()
	}()
	return ctx
}

func run(portMapping []string) error {
	ctx := interrupt()

	forwardedPorts, err := ParsePorts(portMapping)
	if err != nil {
		return err
	}

	args := Args{Name: name, Port: port, ForwardedPorts: forwardedPorts}
	yaml, err := GenerateYAML(path, args)
	if err != nil {
		return err
	}

	client, err := NewClient(namespace)
	if err != nil {
		return err
	}

	if dryRun {
		return client.KubeDump(yaml)
	}

	definitions := map[string]*KubeDef{}
	defer func() { client.KubeDelete(definitions) }() // wrap it in function to ensure variable is evaluated later on
	if definitions, err = client.KubeApply(yaml); err != nil {
		return err
	}

	pod, ok := definitions["Pod"]
	if !ok {
		return errors.New("no pod deployed aborting")
	}

	if err := client.WaitReady(pod); err != nil {
		return err
	}

	if err := client.PortForward(ctx, pod, port); err != nil {
		return err
	}

	if err := Tunnel(ctx, port, forwardedPorts); err != nil {
		return err
	}
	return nil
}
