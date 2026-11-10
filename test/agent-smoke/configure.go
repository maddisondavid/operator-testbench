package main

import (
	"bytes"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
)

// Re-encode only the disposable input. Reject ambiguous YAML before modifying it.
func configure(root, host, port string) error {
	p := filepath.Join(root, "config/operator.yaml")
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	if len(b) > 4<<20 {
		return fmt.Errorf("configuration too large")
	}
	var node yaml.Node
	d := yaml.NewDecoder(bytes.NewReader(b))
	if d.Decode(&node) != nil {
		return fmt.Errorf("invalid configuration YAML")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("multiple configuration documents")
	}
	count := 0
	var check func(*yaml.Node, int) bool
	check = func(n *yaml.Node, depth int) bool {
		count++
		if depth > 64 || count > 100000 {
			return false
		}
		switch n.Kind {
		case yaml.DocumentNode:
			if len(n.Content) != 1 {
				return false
			}
		case yaml.MappingNode:
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || seen[k.Value] {
					return false
				}
				seen[k.Value] = true
			}
		case yaml.SequenceNode:
		case yaml.ScalarNode:
			if n.Tag != "!!str" {
				return false
			}
		default:
			return false
		}
		for _, c := range n.Content {
			if !check(c, depth+1) {
				return false
			}
		}
		return true
	}
	if !check(&node, 0) || node.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("ambiguous configuration YAML")
	}
	var api *yaml.Node
	top := node.Content[0]
	for i := 0; i < len(top.Content); i += 2 {
		if top.Content[i].Value == "api" {
			api = top.Content[i+1]
		}
	}
	if api == nil || api.Kind != yaml.MappingNode {
		return fmt.Errorf("missing API mapping")
	}
	found := 0
	for i := 0; i < len(api.Content); i += 2 {
		k, v := api.Content[i], api.Content[i+1]
		if k.Value == "listen" || k.Value == "advertiseURL" {
			if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
				return fmt.Errorf("invalid endpoint value")
			}
			found++
			if k.Value == "listen" {
				v.Value = "0.0.0.0:" + port
			} else {
				v.Value = "http://" + host + ":" + port
			}
		}
	}
	if found != 2 {
		return fmt.Errorf("missing endpoint fields")
	}
	b, e = yaml.Marshal(&node)
	if e != nil {
		return e
	}
	return os.WriteFile(p, b, 0600)
}
