package app

import (
	"bytes"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"net/url"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/httpserver"
	"operator-dynamo/internal/kubeapi"
	"operator-dynamo/internal/operatorruntime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type inputReader interface{ ReadInput(string) ([]byte, error) }
type eventRecorder interface {
	Record(contracts.Observation) error
}
type connection struct {
	APIVersion string                 `yaml:"apiVersion"`
	Kind       string                 `yaml:"kind"`
	Operator   operatorruntime.Config `yaml:"operator"`
	API        struct {
		Listen          string   `yaml:"listen"`
		AdvertiseURL    string   `yaml:"advertiseURL"`
		CRDs            []string `yaml:"crds"`
		Builtins        []string `yaml:"builtins"`
		RequiredWatches []string `yaml:"requiredWatches"`
		ReadinessMode   string   `yaml:"readinessMode"`
	} `yaml:"api"`
	StartupTimeout      string `yaml:"startupTimeout"`
	requiredWatches     []string
	allNamespaceWatches bool
}

func prepare(options contracts.Options, work Workspace, journal Journal) ([]Component, error) {
	reader, ok := work.(inputReader)
	if !ok {
		return nil, errors.New("workspace input contract unavailable")
	}
	data, err := reader.ReadInput("config/operator.yaml")
	health := httpserver.New(options.Listen)
	if errors.Is(err, os.ErrNotExist) {
		return []Component{health}, nil
	}
	if err != nil {
		return nil, err
	}
	cfg, regs, timeout, err := readConnection(data, reader)
	if err != nil {
		return nil, err
	}
	recorder, ok := journal.(eventRecorder)
	if !ok {
		return nil, errors.New("journal observation contract unavailable")
	}
	api := cfg.newAPI(regs, recorder.Record)
	runtime := operatorruntime.New(cfg.Operator, cfg.API.AdvertiseURL, timeout, api.Connected, recorder.Record)
	if options.Console != nil {
		runtime.Progress = options.Console.Observe
	}
	if inputs, ok := work.(operatorruntime.Inputs); ok {
		runtime.Inputs = inputs
	}
	return []Component{health, api, runtime}, nil
}
func readConnection(data []byte, reader inputReader) (connection, []kubeapi.Registration, time.Duration, error) {
	var cfg connection
	invalid := func() (connection, []kubeapi.Registration, time.Duration, error) {
		return cfg, nil, 0, contracts.ErrInvalid
	}
	var root yaml.Node
	if yaml.Unmarshal(data, &root) != nil {
		return invalid()
	}
	var stringLeaves func(*yaml.Node, int) bool
	stringLeaves = func(n *yaml.Node, depth int) bool {
		if depth > 64 {
			return false
		}
		if n.Kind == yaml.AliasNode {
			return stringLeaves(n.Alias, depth+1)
		}
		if n.Kind == yaml.ScalarNode {
			return n.Tag == "!!str"
		}
		for _, child := range n.Content {
			if !stringLeaves(child, depth+1) {
				return false
			}
		}
		return true
	}
	if !stringLeaves(&root, 0) {
		return invalid()
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return invalid()
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return invalid()
	}
	if cfg.APIVersion != "dynamo/v1alpha1" || cfg.Kind != "OperatorConnection" || cfg.Operator.Validate() != nil {
		return invalid()
	}
	if cfg.StartupTimeout == "" {
		cfg.StartupTimeout = "30s"
	}
	timeout, err := time.ParseDuration(cfg.StartupTimeout)
	if err != nil || timeout <= 0 {
		return invalid()
	}
	host, port, err := net.SplitHostPort(cfg.API.Listen)
	if err != nil || host == "" {
		return invalid()
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return invalid()
	}
	u, err := url.Parse(cfg.API.AdvertiseURL)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.Hostname() == "0.0.0.0" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return invalid()
	}
	if p := u.Port(); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return invalid()
		}
	}
	if len(cfg.API.CRDs) > 32 || len(cfg.API.Builtins) > 7 {
		return invalid()
	}
	regs := []kubeapi.Registration{}
	seenBuiltins := map[string]bool{}
	for _, key := range cfg.API.Builtins {
		reg, ok := kubeapi.Builtin(key)
		if !ok || seenBuiltins[key] {
			return invalid()
		}
		seenBuiltins[key] = true
		regs = append(regs, reg)
	}
	for _, path := range cfg.API.CRDs {
		if path == "" || filepath.IsAbs(path) {
			return invalid()
		}
		clean := filepath.Clean(filepath.Join("config", path))
		if !filepath.IsLocal(clean) || clean == "runs" || strings.HasPrefix(clean, "runs/") {
			return invalid()
		}
		raw, err := reader.ReadInput(clean)
		if err != nil {
			if errors.Is(err, contracts.ErrInvalid) || strings.Contains(err.Error(), "escapes") {
				return invalid()
			}
			return cfg, nil, 0, fmt.Errorf("read CRD input: %w", err)
		}
		imported, err := kubeapi.ImportCRDs(raw)
		if err != nil {
			return invalid()
		}
		regs = append(regs, imported...)
	}
	// The pinned CronJob profile needs the native batch/v1 Job cache. This is a
	// real registered resource with exactly the same limited API-owned semantics.
	for _, r := range regs {
		if r.Group == "batch.tutorial.kubebuilder.io" && r.Kind == "CronJob" {
			if !seenBuiltins["batch/v1/jobs"] {
				job, _ := kubeapi.Builtin("batch/v1/jobs")
				regs = append(regs, job)
			}
			break
		}
	}
	if err := kubeapi.ValidateRegistrations(regs); err != nil || len(regs) == 0 {
		return invalid()
	}
	var presence struct {
		API map[string]yaml.Node `yaml:"api"`
	}
	if err := root.Decode(&presence); err != nil {
		return invalid()
	}
	if _, present := presence.API["readinessMode"]; present && cfg.API.ReadinessMode == "" {
		return invalid()
	}
	if cfg.API.ReadinessMode != "" && cfg.API.ReadinessMode != "list-watch" && cfg.API.ReadinessMode != "initial-watch" {
		return invalid()
	}
	if cfg.API.ReadinessMode == "initial-watch" && len(cfg.API.RequiredWatches) == 0 {
		return invalid()
	}
	catalog := map[string]bool{}
	for _, reg := range regs {
		catalog[reg.Key()] = true
		cfg.requiredWatches = append(cfg.requiredWatches, reg.Key())
	}
	if cfg.API.RequiredWatches != nil {
		if len(cfg.API.RequiredWatches) == 0 || len(cfg.API.RequiredWatches) > 32 {
			return invalid()
		}
		seen := map[string]bool{}
		for _, key := range cfg.API.RequiredWatches {
			if !catalog[key] || seen[key] {
				return invalid()
			}
			seen[key] = true
		}
		cfg.requiredWatches = append([]string(nil), cfg.API.RequiredWatches...)
		cfg.allNamespaceWatches = true
	} else {
		for _, reg := range regs {
			if reg.Group == "batch.tutorial.kubebuilder.io" && reg.Version == "v1" && reg.Kind == "CronJob" {
				cfg.requiredWatches = []string{reg.Key(), "batch/v1/jobs"}
				break
			}
		}
	}
	return cfg, regs, timeout, nil
}

// Both normal and declarative sessions use the same preflight-resolved selection.
func (c connection) newAPI(regs []kubeapi.Registration, record kubeapi.Recorder) *kubeapi.Server {
	api := kubeapi.New(c.API.Listen, regs, record)
	api.RequiredWatches = append([]string(nil), c.requiredWatches...)
	api.RequireAllNamespaces = c.allNamespaceWatches
	api.InitialWatchReadiness = c.API.ReadinessMode == "initial-watch"
	return api
}
