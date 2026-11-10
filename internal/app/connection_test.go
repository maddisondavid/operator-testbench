package app

import (
	"errors"
	"operator-dynamo/internal/contracts"
	"os"
	"strings"
	"testing"
)

type inputs map[string][]byte

func (i inputs) ReadInput(p string) ([]byte, error) {
	if b, ok := i[p]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

const configText = `apiVersion: dynamo/v1alpha1
kind: OperatorConnection
operator:
  image: local
api:
  listen: 0.0.0.0:18081
  advertiseURL: http://198.51.100.10:18081
  crds: [../resources/example.yaml]
`
const crdText = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: widgets.example.test}
spec:
  group: example.test
  names: {kind: Widget, plural: widgets}
  scope: Namespaced
  versions: [{name: v1, served: true, storage: true}]
`

func TestConnectionValidation(t *testing.T) {
	in := inputs{"resources/example.yaml": []byte(crdText)}
	_, regs, timeout, err := readConnection([]byte(configText), in)
	if err != nil || len(regs) != 1 || timeout.Seconds() != 30 {
		t.Fatal(regs, timeout, err)
	}
	for _, text := range []string{configText + "unknown: true\n", strings.Replace(configText, "image: local", "image: 123", 1), strings.Replace(configText, "image: local", "image: local\n  args: [123]", 1), strings.Replace(configText, "../resources/example.yaml", "../../escape", 1), strings.Replace(configText, "image: local", "image: local\n  env: {KUBECONFIG: other}", 1), configText + "startupTimeout: -1s\n", strings.Replace(configText, "http://198.51.100.10:18081", "https://198.51.100.10:18081", 1), strings.Replace(configText, "image: local", "image: local\n  env: {NUMBER: 3}", 1)} {
		_, _, _, err := readConnection([]byte(text), in)
		if !errors.Is(err, contracts.ErrInvalid) {
			t.Fatalf("invalid config accepted: %v", err)
		}
	}
	_, _, _, err = readConnection([]byte(configText), inputs{})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("input I/O classification", err)
	}
}
