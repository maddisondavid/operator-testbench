// Package kubeapi owns registration and all virtual resource semantics.
package kubeapi

import (
	"bytes"
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"operator-dynamo/internal/contracts"
	"regexp"
	"strings"
)

type Registration struct {
	Group, Version, Kind, ListKind, Plural, Singular, Scope string
	Status                                                  bool
}

func (r Registration) APIVersion() string {
	if r.Group == "" {
		return r.Version
	}
	return r.Group + "/" + r.Version
}
func (r Registration) Key() string { return r.APIVersion() + "/" + r.Plural }

var dns = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
var kindName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

func ValidName(s string) bool {
	return len(s) > 0 && len(s) <= 253 && dns.MatchString(s) && !strings.Contains(s, "..")
}
func Invalid() error { return contracts.ErrInvalid }

type crd struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Group string `yaml:"group"`
		Scope string `yaml:"scope"`
		Names struct {
			Kind     string `yaml:"kind"`
			ListKind string `yaml:"listKind"`
			Plural   string `yaml:"plural"`
			Singular string `yaml:"singular"`
		} `yaml:"names"`
		Versions []struct {
			Name         yaml.Node            `yaml:"name"`
			Served       yaml.Node            `yaml:"served"`
			Storage      yaml.Node            `yaml:"storage"`
			Subresources map[string]yaml.Node `yaml:"subresources"`
		} `yaml:"versions"`
		Conversion *struct {
			Strategy string `yaml:"strategy"`
			Webhook  any    `yaml:"webhook"`
		} `yaml:"conversion"`
	} `yaml:"spec"`
}

// ImportCRDs reads native CRD documents, without executing schemas or other CRD behavior.
func ImportCRDs(data []byte) ([]Registration, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []Registration
	for {
		var c crd
		err := dec.Decode(&c)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, Invalid()
		}
		if c.APIVersion != "apiextensions.k8s.io/v1" || c.Kind != "CustomResourceDefinition" || len(c.Spec.Versions) == 0 {
			return nil, Invalid()
		}
		selected := -1
		seen := map[string]bool{}
		for i, version := range c.Spec.Versions {
			if version.Name.Kind != yaml.ScalarNode || version.Name.Tag != "!!str" {
				return nil, Invalid()
			}
			name := version.Name.Value
			served, servedOK := versionFlag(version.Served)
			storage, storageOK := versionFlag(version.Storage)
			if !servedOK || !storageOK || !ValidName(name) || strings.Contains(name, ".") || seen[name] || served != storage {
				return nil, Invalid()
			}
			seen[name] = true
			if served {
				if selected != -1 {
					return nil, Invalid()
				}
				selected = i
			}
		}
		if selected == -1 {
			return nil, Invalid()
		}
		v := c.Spec.Versions[selected]
		n := c.Spec.Names
		if !ValidName(c.Spec.Group) || !strings.Contains(c.Spec.Group, ".") || !ValidName(n.Plural) || strings.Contains(n.Plural, ".") || !kindName.MatchString(n.Kind) || c.Metadata.Name != n.Plural+"."+c.Spec.Group {
			return nil, Invalid()
		}
		if c.Spec.Scope != "Namespaced" && c.Spec.Scope != "Cluster" {
			return nil, Invalid()
		}
		if c.Spec.Conversion != nil && (c.Spec.Conversion.Strategy != "None" || c.Spec.Conversion.Webhook != nil) {
			return nil, Invalid()
		}
		if n.ListKind == "" {
			n.ListKind = n.Kind + "List"
		}
		if !kindName.MatchString(n.ListKind) {
			return nil, Invalid()
		}
		if n.Singular == "" {
			n.Singular = strings.ToLower(n.Kind)
		}
		enabled := false
		if node, ok := v.Subresources["status"]; ok {
			if node.Kind != yaml.MappingNode || len(node.Content) != 0 {
				return nil, Invalid()
			}
			enabled = true
		}
		out = append(out, Registration{c.Spec.Group, v.Name.Value, n.Kind, n.ListKind, n.Plural, n.Singular, c.Spec.Scope, enabled})
	}
	if len(out) == 0 {
		return nil, Invalid()
	}
	return out, nil
}

// Presence and YAML type matter: absent/null/quoted booleans are not inactive flags.
func versionFlag(node yaml.Node) (bool, bool) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
		return false, false
	}
	var value bool
	if node.Decode(&value) != nil {
		return false, false
	}
	return value, true
}
func ValidateRegistrations(regs []Registration) error {
	gvr, gvk := map[string]bool{}, map[string]bool{}
	for _, r := range regs {
		a, b := r.Key(), r.APIVersion()+"/"+r.Kind
		if gvr[a] || gvk[b] {
			return Invalid()
		}
		gvr[a] = true
		gvk[b] = true
	}
	return nil
}

const RegistrationNotice = "metadata-only CRD import: exactly one served/storage version; additional versions must explicitly be unserved/nonstorage and have no routes or capabilities; schemas, validation, pruning, defaults, CEL, admission, conversion and scale are not executed; only selected-version declared empty status is served; explicit DELETE propagation is unsupported and no garbage collector runs; descriptive/additional fields do not enable capabilities"

// Builtin returns API-owned metadata for the closed supported builtin catalog.
func Builtin(key string) (Registration, bool) {
	switch key {
	case "v1/namespaces":
		return Registration{Version: "v1", Kind: "Namespace", ListKind: "NamespaceList", Plural: "namespaces", Singular: "namespace", Scope: "Cluster"}, true
	case "v1/secrets":
		return Registration{Version: "v1", Kind: "Secret", ListKind: "SecretList", Plural: "secrets", Singular: "secret", Scope: "Namespaced"}, true
	case "v1/configmaps":
		return Registration{Version: "v1", Kind: "ConfigMap", ListKind: "ConfigMapList", Plural: "configmaps", Singular: "configmap", Scope: "Namespaced"}, true
	case "apps/v1/deployments", "apps/v1/daemonsets", "apps/v1/statefulsets":
		plural := strings.TrimPrefix(key, "apps/v1/")
		kind := map[string]string{"deployments": "Deployment", "daemonsets": "DaemonSet", "statefulsets": "StatefulSet"}[plural]
		return Registration{Group: "apps", Version: "v1", Kind: kind, ListKind: kind + "List", Plural: plural, Singular: strings.ToLower(kind), Scope: "Namespaced"}, true
	case "batch/v1/jobs":
		return Registration{Group: "batch", Version: "v1", Kind: "Job", ListKind: "JobList", Plural: "jobs", Singular: "job", Scope: "Namespaced"}, true
	}
	return Registration{}, false
}
