package main

import (
	"context"
	"fmt"
	"gopkg.in/yaml.v3"
	"operator-dynamo/internal/kubeapi"
	"operator-dynamo/internal/operatorruntime"
	"operator-dynamo/internal/scenario"
	"operator-dynamo/internal/workspace"
	"path/filepath"
	"time"
)

// Reconstruct input/selection identity with the existing planners, without
// launching Docker or executing the test again. Configuration admission remains
// the CLI's responsibility; this adapter extracts only planner inputs.
func expected(root, selection string) (*scenario.Plan, map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w, e := workspace.Open(root)
	if e != nil {
		return nil, nil, e
	}
	defer w.Close()
	b, e := w.ReadSource(ctx, "config/operator.yaml", 4<<20)
	if e != nil {
		return nil, nil, e
	}
	var cfg struct {
		Operator operatorruntime.Config `yaml:"operator"`
		API      struct {
			CRDs     []string `yaml:"crds"`
			Builtins []string `yaml:"builtins"`
		} `yaml:"api"`
	}
	if e = yaml.Unmarshal(b.Data, &cfg); e != nil {
		return nil, nil, e
	}
	regs := []kubeapi.Registration{}
	paths := []string{"config/operator.yaml"}
	for _, key := range cfg.API.Builtins {
		r, ok := kubeapi.Builtin(key)
		if !ok {
			return nil, nil, fmt.Errorf("unknown registration")
		}
		regs = append(regs, r)
	}
	for _, p := range cfg.API.CRDs {
		p = filepath.Clean(filepath.Join("config", p))
		b, e := w.ReadSource(ctx, p, 4<<20)
		if e != nil {
			return nil, nil, e
		}
		r, e := kubeapi.ImportCRDs(b.Data)
		if e != nil {
			return nil, nil, e
		}
		regs = append(regs, r...)
		paths = append(paths, p)
	}
	for _, r := range regs {
		if r.Group == "batch.tutorial.kubebuilder.io" && r.Kind == "CronJob" {
			found := false
			for _, r := range regs {
				if r.Group == "batch" && r.Kind == "Job" {
					found = true
				}
			}
			if !found {
				j, _ := kubeapi.Builtin("batch/v1/jobs")
				regs = append(regs, j)
			}
			break
		}
	}
	resources := []scenario.Resource{}
	for _, r := range regs {
		resources = append(resources, scenario.Resource{APIVersion: r.APIVersion(), Kind: r.Kind, ListKind: r.ListKind, Plural: r.Plural, Scope: r.Scope})
	}
	plan, e := scenario.BuildPlan(ctx, w, selection, resources)
	if e != nil {
		return nil, nil, e
	}
	prepared, e := operatorruntime.Prepare(ctx, cfg.Operator, w)
	if e != nil {
		return nil, nil, e
	}
	hashes := map[string]string{}
	for _, in := range append(plan.Inputs, prepared.Manifest()...) {
		hashes[in.Path] = in.SHA256
	}
	for _, p := range paths {
		b, e := w.ReadSource(ctx, p, 4<<20)
		if e != nil {
			return nil, nil, e
		}
		hashes[p] = digest(b.Data)
	}
	return plan, hashes, nil
}
