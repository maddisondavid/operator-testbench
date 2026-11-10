package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var imageIdentity = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func provenance(root, artifact, sid string, started time.Time) error {
	b, e := os.ReadFile(filepath.Join(root, artifact, "session.json"))
	if e != nil {
		return fmt.Errorf("missing session identity")
	}
	var session struct {
		ID, State string
		StartedAt time.Time
	}
	if json.Unmarshal(b, &session) != nil || session.ID != sid || session.State != "stopped" || session.StartedAt.Before(started) {
		return fmt.Errorf("session provenance mismatch")
	}
	b, e = os.ReadFile(filepath.Join(root, "config/operator.yaml"))
	if e != nil {
		return e
	}
	var cfg struct {
		Operator struct {
			Build any `yaml:"build"`
		} `yaml:"operator"`
	}
	if yaml.Unmarshal(b, &cfg) != nil {
		return fmt.Errorf("invalid configuration")
	}
	image, container, buildImage := "", "", ""
	f, e := os.Open(filepath.Join(root, artifact, "events.jsonl"))
	if e != nil {
		return e
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var total int64
	removed := false
	for scanner.Scan() {
		total += int64(len(scanner.Bytes()))
		if total > 128<<20 {
			return fmt.Errorf("journal evidence too large")
		}
		var event struct {
			SessionID, Kind string
			Payload         struct{ ImageID, ContainerID string }
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.SessionID != sid {
			return fmt.Errorf("journal provenance mismatch")
		}
		switch event.Kind {
		case "operator.started":
			image = event.Payload.ImageID
			container = event.Payload.ContainerID
		case "operator.build.completed":
			buildImage = event.Payload.ImageID
		case "operator.removed":
			if event.Payload.ImageID == image && event.Payload.ContainerID == container && container != "" {
				removed = true
			}
		}
	}
	if scanner.Err() != nil || !imageIdentity.MatchString(image) || !removed {
		return fmt.Errorf("missing operator image/cleanup evidence")
	}
	if cfg.Operator.Build != nil {
		b, e := os.ReadFile(filepath.Join(root, artifact, "operator-build.json"))
		if e != nil || len(b) > 1<<20 {
			return fmt.Errorf("missing build evidence")
		}
		var v struct {
			Outcome       string
			ImageID       string
			ContextSHA256 string
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		if dec.Decode(&v) != nil || v.Outcome != "completed" || v.ImageID != image || buildImage != image || !imageIdentity.MatchString("sha256:"+v.ContextSHA256) {
			return fmt.Errorf("build provenance mismatch")
		}
	}
	return nil
}
