package operatorruntime

import (
	"operator-dynamo/internal/contracts"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type BuildConfig struct {
	Context    string `yaml:"context"`
	Dockerfile string `yaml:"dockerfile"`
	Timeout    string `yaml:"timeout"`
}
type FileConfig struct {
	Source string `yaml:"source"`
	Path   string `yaml:"path"`
}
type VolumeConfig struct {
	Name string `yaml:"name"`
}
type LogConfig struct {
	Mode   string   `yaml:"mode"`
	Redact []string `yaml:"redact"`
}
type Config struct {
	Image   string            `yaml:"image"`
	Build   *BuildConfig      `yaml:"build"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	Files   []FileConfig      `yaml:"files"`
	Volumes []VolumeConfig    `yaml:"volumes"`
	Logs    LogConfig         `yaml:"logs"`
}

func relative(name string) bool {
	return name != "" && name != "." && path.Clean(name) == name && !strings.ContainsAny(name, "\\\x00\r\n") && filepath.IsLocal(name)
}
func sourcePath(name string) (string, bool) {
	if name == "" || filepath.IsAbs(name) || strings.ContainsAny(name, "\\\x00") {
		return "", false
	}
	p := filepath.Clean(filepath.Join("config", name))
	return p, filepath.IsLocal(p)
}
func (c Config) Validate() error {
	invalid := contracts.ErrInvalid
	if (strings.TrimSpace(c.Image) == "") == (c.Build == nil) {
		return invalid
	}
	if strings.ContainsRune(c.Image, 0) {
		return invalid
	}
	if c.Build != nil {
		p, ok := sourcePath(c.Build.Context)
		if !ok || p == "." {
			return invalid
		}
		for _, managed := range []string{"config", "resources", "sessions", "runs"} {
			if p == managed || strings.HasPrefix(p, managed+"/") || strings.HasPrefix(managed, p+"/") {
				return invalid
			}
		}
		if c.Build.Dockerfile != "" && !relative(c.Build.Dockerfile) {
			return invalid
		}
		if c.Build.Timeout != "" {
			d, e := time.ParseDuration(c.Build.Timeout)
			if e != nil || d <= 0 {
				return invalid
			}
		}
	}
	for k, v := range c.Env {
		if k == "KUBECONFIG" || k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return invalid
		}
	}
	for _, a := range c.Args {
		if strings.ContainsRune(a, 0) {
			return invalid
		}
	}
	if len(c.Files) > 32 || len(c.Volumes) > 8 {
		return invalid
	}
	names := map[string]bool{}
	for _, f := range c.Files {
		if p, ok := sourcePath(f.Source); !ok || p == "runs" || strings.HasPrefix(p, "runs/") {
			return invalid
		}
		if !relative(f.Path) {
			return invalid
		}
		for p := range names {
			if p == f.Path || strings.HasPrefix(p, f.Path+"/") || strings.HasPrefix(f.Path, p+"/") {
				return invalid
			}
		}
		names[f.Path] = true
	}
	names = map[string]bool{}
	label := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	for _, v := range c.Volumes {
		if len(v.Name) > 63 || !label.MatchString(v.Name) || names[v.Name] {
			return invalid
		}
		names[v.Name] = true
	}
	if c.Logs.Mode != "" && c.Logs.Mode != "off" && c.Logs.Mode != "capture" {
		return invalid
	}
	if c.Logs.Mode != "capture" {
		if len(c.Logs.Redact) > 0 {
			return invalid
		}
		return nil
	}
	patterns := c.patterns()
	total := 0
	if len(patterns) > 128 {
		return invalid
	}
	for _, p := range patterns {
		if p == "" || len(p) > 4096 {
			return invalid
		}
		total += len(p)
	}
	if total > 64<<10 {
		return invalid
	}
	return nil
}
func (c Config) patterns() []string {
	p := append([]string(nil), c.Logs.Redact...)
	for _, v := range c.Env {
		if v != "" {
			p = append(p, v)
		}
	}
	return p
}
