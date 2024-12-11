package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mdobak/go-xerrors"
	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

type Client struct {
	namespace string
	*dynamic.DynamicClient
	*rest.Config
	clientcmd.ClientConfig
	Mapper *restmapper.DeferredDiscoveryRESTMapper
}

func (c *Client) DefaultNS(kd *KubeDef) error {
	if ns := kd.GetNamespace(); ns != "" {
		return nil
	}
	if c.namespace != "" {
		kd.SetNamespace(c.namespace)
		return nil
	} else {
		ns, _, err := c.ClientConfig.Namespace()
		kd.SetNamespace(ns)
		return err
	}
}

func NewClient(namespace string) (*Client, error) {
	localCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)
	restCfg, err := localCfg.ClientConfig()
	if err != nil {
		return nil, err
	}

	// 1. Prepare a RESTMapper to find GVR
	dc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(
		memory.NewMemCacheClient(dc),
	)

	// 2. Prepare the dynamic client
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	return &Client{
		Mapper:        mapper,
		DynamicClient: client,
		ClientConfig:  localCfg,
		Config:        restCfg,
		namespace:     namespace,
	}, nil
}

func (c *Client) WaitReady(pod *KubeDef) error {
	obj, err := c.Resource(schema.GroupVersionResource{
		Version:  "v1",
		Resource: "pods",
	}).Namespace(pod.GetNamespace()).Watch(context.Background(), metav1.ListOptions{
		FieldSelector: "metadata.name=" + pod.GetName(),
	})
	if err != nil {
		return err
	}
	for event := range obj.ResultChan() {
		fmt.Println("waiting for pod to get ready")
		phase, ok, err := unstructured.NestedString(
			event.Object.(*unstructured.Unstructured).Object,
			"status", "phase")
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if phase == "Running" {
			break
		}
	}
	return nil
}

func (c *Client) dynamicResource(kd *KubeDef) (dr dynamic.ResourceInterface, err error) {
	// 4. Find GVR
	gvk := kd.GetObjectKind().GroupVersionKind()
	mapping, err := c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}

	// 5. Obtain REST interface for the GVR
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		// namespaced resources should specify the namespace
		if err := c.DefaultNS(kd); err != nil {
			return nil, err
		}
		dr = c.Resource(mapping.Resource).Namespace(kd.GetNamespace())
	} else {
		kd.SetNamespace("")
		// for cluster-wide resources
		dr = c.Resource(mapping.Resource)
	}
	return dr, nil
}

func (c *Client) KubeApply(yaml *bytes.Buffer) (map[string]*KubeDef, error) {
	kds := map[string]*KubeDef{}

	for kd, err := range YieldKubeDef(yaml) {
		if err != nil {
			return kds, err
		}
		dr, err := c.dynamicResource(kd)
		if err != nil {
			return kds, err
		}
		fm := metav1.ApplyOptions{FieldManager: name}
		fmt.Printf("applying %s - %s\n", strings.ToLower(kd.GetKind()), kd.GetName())
		if _, err := dr.Apply(context.Background(), kd.GetName(), kd, fm); err != nil {
			return nil, err
		}
		kds[kd.GetKind()] = kd
	}
	return kds, nil
}

func (c *Client) KubeDump(buffer *bytes.Buffer) error {
	for kd, err := range YieldKubeDef(buffer) {
		if err != nil {
			return err
		}
		if _, err := c.dynamicResource(kd); err != nil {
			return err
		}
		buf, err := yaml.Marshal(kd.Object)
		if err != nil {
			return err
		}
		fmt.Printf("---\n%s", buf)
	}
	return nil
}

func (c *Client) KubeDelete(kds map[string]*KubeDef) error {
	errs := []error{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	propagation := metav1.DeletePropagationForeground
	deleteOpts := metav1.DeleteOptions{PropagationPolicy: &propagation}
	defer cancel()
	for _, kd := range kds {
		dr, err := c.dynamicResource(kd)
		if err != nil {
			return err
		}
		fmt.Printf("deleting %s - %s\n", strings.ToLower(kd.GetKind()), kd.GetName())
		errs = append(errs, dr.Delete(ctx, kd.GetName(), deleteOpts))
	}
	return xerrors.Append(nil, errs...)
}

func (c *Client) PortForward(ctx context.Context, pod *KubeDef, port int) error {
	roundTripper, upgrader, err := spdy.RoundTripperFor(c.Config)
	if err != nil {
		return err
	}

	path := fmt.Sprintf(
		"/api/v1/namespaces/%s/pods/%s/portforward",
		pod.GetNamespace(),
		pod.GetName(),
	)
	hostIP := strings.TrimLeft(c.Config.Host, "htps:/")
	serverURL := url.URL{Scheme: "https", Path: path, Host: hostIP}

	dialer := spdy.NewDialer(
		upgrader,
		&http.Client{Transport: roundTripper},
		http.MethodPost,
		&serverURL,
	)

	ready, done := make(chan struct{}, 1), ctx.Done()
	ports := []string{fmt.Sprintf("%d:%d", port, port)}
	errOut := new(bytes.Buffer)
	errChan := make(chan error)
	// defer close(errChan)

	// we do not want to retrieve port forward output errOut,
	forwarder, err := portforward.New(dialer, ports, done, ready, nil, errOut)
	if err != nil {
		return err
	}

	go func() { errChan <- forwarder.ForwardPorts() }()
	select {
	case <-ready:
		// we only open one port, there should be no error stored elsewhere,
		// check ForwardPorts implementation for details
		return nil
	case err := <-errChan:
		if errors.Is(err, portforward.ErrLostConnectionToPod) {
			return err
		}
		return xerrors.New(errOut.String())
	}
}
