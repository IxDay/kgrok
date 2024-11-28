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

	path    string
	decoder = yaml.NewDecodingSerializer(
		unstructured.UnstructuredJSONScheme,
	)
	command = &cobra.Command{
		Use:   name + " [flags]",
		Short: ".",
		Args:  cobra.MinimumNArgs(1), // we want at least one port mapping
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

	client, err := NewClient()
	if err != nil {
		return err
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
