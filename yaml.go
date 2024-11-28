package main

import (
	"bufio"
	"bytes"
	"io"
	"iter"
	"os"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

type KubeDef = unstructured.Unstructured

var tplt = []byte(`
---
apiVersion: v1
kind: Service
metadata:
  name: {{.Name}}
  labels:
    app: {{.Name}}
spec:
  selector:
    app: {{.Name}}
  ports:
  {{- range $_, $fp := .ForwardedPorts}}
  - name: tcp-{{$fp.Remote}}
    port: {{$fp.Remote}}
    targetPort: {{$fp.Remote}}
  {{- end}}
---
apiVersion: v1
kind: Pod
metadata:
  name: {{.Name}}
  labels:
    app: {{.Name}}
spec:
  containers:
  - command: ["/ktunnel/ktunnel", "server", "-p", "{{.Port}}"]
    image: docker.io/omrieival/ktunnel:v1.6.1
    name: {{.Name}}
`)

type Args struct {
	Port           int
	Name           string
	ForwardedPorts []ForwardedPort
}

func GenerateYAML(path string, args Args) (*bytes.Buffer, error) {
	// https://stackoverflow.com/questions/22563616/determine-if-stdin-has-data-with-go#answer-22564526
	info, err := os.Stdin.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > 0 {
		tplt, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
	} else if path != "" {
		tplt, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}
	buffer := bytes.NewBuffer(nil)
	return buffer, template.
		Must(template.New("base").Funcs(sprig.FuncMap()).Parse(string(tplt))).
		Execute(buffer, args)
}

func YieldKubeDef(buffer *bytes.Buffer) iter.Seq2[*KubeDef, error] {
	multidocReader := yaml.NewYAMLReader(bufio.NewReader(buffer))
	return func(yield func(*KubeDef, error) bool) {
		for {
			buf, err := multidocReader.Read()
			if err != nil {
				if err != io.EOF {
					yield(nil, err)
				}
				return
			} else if buf[0] == 10 { // empty YAML (this is line break)
				continue
			}
			// 3. Decode YAML manifest into unstructured.Unstructured
			kd := KubeDef{}
			if _, _, err := decoder.Decode(buf, nil, &kd); err != nil {
				yield(&kd, err)
				return
			}
			if !yield(&kd, err) {
				return
			}
		}
	}
}
