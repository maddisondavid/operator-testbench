package app

import (
	"strings"
	"testing"
)

func TestCoreConnectionSelection(t *testing.T) {
	base := strings.Replace(configText, "  crds: [../resources/example.yaml]", "  crds: []\n  builtins: [v1/namespaces, v1/configmaps]\n  requiredWatches: [v1/namespaces, v1/configmaps]", 1)
	cfg, regs, _, err := readConnection([]byte(base), inputs{})
	if err != nil || len(regs) != 2 || !cfg.newAPI(regs, nil).RequireAllNamespaces || strings.Join(cfg.requiredWatches, ",") != "v1/namespaces,v1/configmaps" {
		t.Fatal(cfg, regs, err)
	}
	for _, change := range [][2]string{
		{"[v1/namespaces, v1/configmaps]", "null"},
		{"builtins: [v1/namespaces, v1/configmaps]", "builtins: v1/configmaps"},
		{"builtins: [v1/namespaces, v1/configmaps]", "builtins: [v1/configmaps, v1/configmaps]"},
		{"builtins: [v1/namespaces, v1/configmaps]", "builtins: [v1/pods]"},
		{"builtins: [v1/namespaces, v1/configmaps]", "builtins: []"},
		{"requiredWatches: [v1/namespaces, v1/configmaps]", "requiredWatches: []"},
		{"requiredWatches: [v1/namespaces, v1/configmaps]", "requiredWatches: null"},
		{"requiredWatches: [v1/namespaces, v1/configmaps]", "requiredWatches: [v1/configmaps, v1/configmaps]"},
		{"requiredWatches: [v1/namespaces, v1/configmaps]", "requiredWatches: [batch/v1/jobs]"},
		{"requiredWatches: [v1/namespaces, v1/configmaps]", "requiredWatches: [true]"},
		{"requiredWatches: [v1/namespaces, v1/configmaps]", "requiredWatches: {}"},
	} {
		text := strings.Replace(base, change[0], change[1], 1)
		if _, _, _, err := readConnection([]byte(text), inputs{}); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
	legacy, regs, _, err := readConnection([]byte(configText), inputs{"resources/example.yaml": []byte(crdText)})
	if err != nil || legacy.newAPI(regs, nil).RequireAllNamespaces || len(legacy.requiredWatches) != 1 {
		t.Fatal(legacy, err)
	}
	tutorial := strings.ReplaceAll(strings.ReplaceAll(crdText, "example.test", "batch.tutorial.kubebuilder.io"), "Widget", "CronJob")
	for _, extra := range []string{"", "  builtins: [batch/v1/jobs]\n"} {
		c, r, _, e := readConnection([]byte(configText+extra), inputs{"resources/example.yaml": []byte(tutorial)})
		if e != nil || len(r) != 2 || strings.Join(c.requiredWatches, ",") != "batch.tutorial.kubebuilder.io/v1/widgets,batch/v1/jobs" {
			t.Fatal(c, r, e)
		}
	}
}

func TestWorkloadConnectionCatalog(t *testing.T) {
	text := strings.Replace(configText, "  crds: [../resources/example.yaml]", "  crds: []\n  builtins: [v1/namespaces, v1/configmaps, batch/v1/jobs, apps/v1/deployments, apps/v1/daemonsets, apps/v1/statefulsets]\n  requiredWatches: [v1/configmaps]", 1)
	cfg, regs, _, err := readConnection([]byte(text), inputs{})
	if err != nil || len(regs) != 6 || len(cfg.requiredWatches) != 1 {
		t.Fatal(regs, err)
	}
	for _, bad := range []string{strings.Replace(text, "apps/v1/deployments", "apps/v1/deployments, apps/v1/deployments", 1), strings.Replace(text, "apps/v1/deployments", "apps/v1/pods", 1)} {
		if _, _, _, err := readConnection([]byte(bad), inputs{}); err == nil {
			t.Fatal("accepted invalid workload catalog")
		}
	}
}

func TestInitialWatchConfiguration(t *testing.T) {
	base := strings.Replace(configText, "  crds: [../resources/example.yaml]", "  crds: []\n  builtins: [v1/namespaces, v1/configmaps, v1/secrets]\n  requiredWatches: [v1/namespaces, v1/configmaps, v1/secrets]\n  readinessMode: initial-watch", 1)
	c, regs, _, err := readConnection([]byte(base), inputs{})
	if err != nil || !c.newAPI(regs, nil).InitialWatchReadiness || !c.newAPI(regs, nil).RequireAllNamespaces {
		t.Fatal(c, err)
	}
	for _, mode := range []string{"null", "false", "[]", "{}", "unknown", `""`} {
		if _, _, _, err := readConnection([]byte(strings.Replace(base, "readinessMode: initial-watch", "readinessMode: "+mode, 1)), inputs{}); err == nil {
			t.Fatal("accepted", mode)
		}
	}
	absent := strings.Replace(base, "  requiredWatches: [v1/namespaces, v1/configmaps, v1/secrets]\n", "", 1)
	if _, _, _, err := readConnection([]byte(absent), inputs{}); err == nil {
		t.Fatal("initial without explicit profile")
	}
}
