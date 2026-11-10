package operatorruntime

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"operator-dynamo/internal/contracts"
)

type Inputs interface {
	ReadSource(context.Context, string, int64) (contracts.InputFile, error)
	Snapshot(context.Context, string, func(string, bool) (bool, error)) ([]contracts.InputFile, error)
	WriteOutput(context.Context, string, []byte) error
	AppendOutput(context.Context, string, []byte) error
}
type StreamEngine interface {
	Stream(context.Context, string, string, io.Reader, string) (io.ReadCloser, error)
}
type buildEvidence struct {
	TransportDockerfile     string            `json:"transportDockerfile"`
	PreparationNS           int64             `json:"preparationNs"`
	Outcome                 string            `json:"outcome"`
	DurationNS              int64             `json:"durationNs"`
	GeneratedPolicyArtifact string            `json:"generatedControlPolicyArtifact"`
	ContextDigest           string            `json:"contextSHA256"`
	Dockerfile              string            `json:"dockerfile"`
	DockerfileDigest        string            `json:"dockerfileSHA256"`
	OriginalControls        map[string]string `json:"originalControlSHA256"`
	GeneratedPolicyDigest   string            `json:"generatedPolicySHA256"`
	Tag                     string            `json:"tag"`
	Image                   string            `json:"imageID"`
	EngineVersion           string            `json:"engineVersion"`
	Profile                 string            `json:"profile"`
}

func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func literalPattern(p string) string {
	var b strings.Builder
	for _, r := range p {
		if unicode.IsSpace(r) {
			b.WriteRune('[')
			b.WriteRune(r)
			b.WriteRune(']')
			continue
		}
		if strings.ContainsRune(`\*?[]!#`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
func (r *Runtime) prepareInputs(ctx context.Context) ([]byte, error) {
	if len(r.Config.Files) > 0 || r.Config.Build != nil || r.Config.Logs.Mode == "capture" {
		if r.Inputs == nil {
			return nil, errors.New("workspace runtime input contract unavailable")
		}
	}
	total := 0
	for _, f := range r.Config.Files {
		source, _ := sourcePath(f.Source)
		v, e := r.Inputs.ReadSource(ctx, source, 4<<20)
		if e != nil {
			return nil, e
		}
		total += len(v.Data)
		if total > 16<<20 {
			return nil, contracts.ErrInvalid
		}
		v.Name = f.Path
		r.inputFiles = append(r.inputFiles, v)
	}
	if r.Config.Build == nil {
		return nil, nil
	}
	cfg := r.Config.Build
	contextPath, _ := sourcePath(cfg.Context)
	dockerfile := cfg.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	controls := map[string][]byte{}
	for _, p := range []string{".dockerignore", dockerfile + ".dockerignore"} {
		v, e := r.Inputs.ReadSource(ctx, path.Join(contextPath, p), 256<<20)
		if e == nil {
			controls[p] = v.Data
		} else if !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
	}
	v, e := r.Inputs.ReadSource(ctx, path.Join(contextPath, dockerfile), 256<<20)
	if e != nil {
		return nil, e
	}
	effective := controls[".dockerignore"]
	if b, ok := controls[dockerfile+".dockerignore"]; ok {
		effective = b
	}
	patterns, e := ignorefile.ReadAll(bytes.NewReader(effective))
	if e != nil {
		return nil, contracts.ErrInvalid
	}
	matcher, e := patternmatcher.New(patterns)
	if e != nil {
		return nil, contracts.ErrInvalid
	}
	files, e := r.Inputs.Snapshot(ctx, contextPath, func(p string, isDir bool) (bool, error) {
		if p == dockerfile || p == ".dockerignore" || p == dockerfile+".dockerignore" {
			return false, nil
		}
		ignored, e := matcher.MatchesOrParentMatches(p)
		if e != nil {
			return false, contracts.ErrInvalid
		}
		return !ignored, nil
	})
	if e != nil {
		return nil, e
	}
	transportDockerfile := dockerfile
	if dockerfile == ".dockerignore" {
		for index := 0; ; index++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			candidate := ".dynamo-control-Dockerfile"
			if index > 0 {
				candidate += fmt.Sprintf("-%d", index)
			}
			collision := candidate == dockerfile || candidate == dockerfile+".dockerignore" || candidate == ".dockerignore"
			for _, f := range files {
				if f.Name == candidate || strings.HasPrefix(f.Name, candidate+"/") || strings.HasPrefix(candidate, f.Name+"/") {
					collision = true
					break
				}
			}
			if !collision {
				transportDockerfile = candidate
				break
			}
		}
	}
	generated := strings.Join(patterns, "\n") + "\n" + literalPattern(dockerfile) + "\n.dockerignore\n" + literalPattern(dockerfile+".dockerignore") + "\n" + literalPattern(transportDockerfile) + "\n"
	files = append(files, contracts.InputFile{Name: transportDockerfile, Mode: 0644, Data: v.Data}, contracts.InputFile{Name: ".dockerignore", Mode: 0644, Data: []byte(generated)})
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	totalBytes := 0
	seenPaths := map[string]bool{}
	for _, f := range files {
		if seenPaths[f.Name] {
			return nil, contracts.ErrInvalid
		}
		seenPaths[f.Name] = true
		totalBytes += len(f.Data)
	}
	if len(files) > 20000 || totalBytes > 256<<20 {
		return nil, contracts.ErrInvalid
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, f := range files {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		header := &tar.Header{Name: f.Name, Mode: f.Mode, Size: int64(len(f.Data))}
		if f.Dir {
			header.Name += "/"
			header.Typeflag = tar.TypeDir
			header.Size = 0
		}
		if e := tw.WriteHeader(header); e != nil {
			return nil, e
		}
		if _, e := tw.Write(f.Data); e != nil {
			return nil, e
		}
	}
	if e := tw.Close(); e != nil {
		return nil, e
	}
	evidence := buildEvidence{ContextDigest: digest(archive.Bytes()), Dockerfile: dockerfile, TransportDockerfile: transportDockerfile, DockerfileDigest: digest(v.Data), OriginalControls: map[string]string{}, GeneratedPolicyDigest: digest([]byte(generated)), GeneratedPolicyArtifact: "operator-build-policy.txt", Outcome: "prepared", Profile: "Engine API1.47 classic version1"}
	for p, b := range controls {
		evidence.OriginalControls[p] = digest(b)
	}
	r.buildInfo = &evidence
	if e := r.Inputs.WriteOutput(ctx, "operator-build-policy.txt", []byte(generated)); e != nil {
		return nil, e
	}
	return archive.Bytes(), nil
}
func (r *Runtime) build(ctx context.Context, archive []byte) (result string, resultErr error) {
	start := time.Now()
	defer func() {
		r.buildInfo.DurationNS = time.Since(start).Nanoseconds()
		if resultErr != nil && r.buildInfo.Outcome != "completed" {
			r.buildInfo.Outcome = "failed"
			if ctx.Err() != nil {
				r.buildInfo.Outcome = "cancelled-daemon-outcome-unconfirmed"
			}
			_ = r.emit("build.failed")
		}
	}()
	r.buildInfo.Outcome = "building"
	if e := r.emit("build.started"); e != nil {
		return "", e
	}

	stream, ok := r.Engine.(StreamEngine)
	if !ok {
		return "", errors.New("Docker build streaming unavailable")
	}
	var version struct{ Version string }
	if e := r.Engine.Call(ctx, "GET", "/version", nil, &version); e != nil {
		return "", e
	}
	r.buildInfo.EngineVersion = version.Version
	tag := "dynamo-build:" + strings.ToLower(r.session)
	r.buildInfo.Tag = tag
	var prior struct {
		ID string `json:"Id"`
	}
	if e := r.Engine.Call(ctx, "GET", "/images/"+url.PathEscape(tag)+"/json", nil, &prior); e == nil {
		return "", errors.New("generated build tag already exists")
	} else if !IsNotFound(e) {
		return "", e
	}
	params := url.Values{"version": {"1"}, "dockerfile": {r.buildInfo.TransportDockerfile}, "t": {tag}, "pull": {"0"}, "rm": {"1"}, "forcerm": {"1"}}
	body, e := stream.Stream(ctx, "POST", "/build?"+params.Encode(), bytes.NewReader(archive), "application/x-tar")
	if e != nil {
		return "", e
	}
	defer body.Close()
	image, e := readBuildResult(body)
	if e != nil {
		return "", e
	}
	if ctx.Err() != nil {
		return "", errors.New("build cancelled; daemon outcome unconfirmed")
	}
	var resolved struct {
		ID string `json:"Id"`
	}
	if e := r.Engine.Call(ctx, "GET", "/images/"+url.PathEscape(tag)+"/json", nil, &resolved); e != nil {
		return "", e
	}
	if resolved.ID != image {
		return "", errors.New("build result identity mismatch")
	}
	r.buildInfo.Image = image
	r.buildInfo.Outcome = "completed"
	r.buildInfo.DurationNS = time.Since(start).Nanoseconds()
	data, _ := json.MarshalIndent(r.buildInfo, "", "  ")
	if e := r.Inputs.WriteOutput(ctx, "operator-build.json", data); e != nil {
		return "", e
	}
	return image, r.emit("build.completed")
}
func readBuildResult(reader io.Reader) (string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), (1<<20)+1)
	image := ""
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > 1<<20 {
			return "", errors.New("build record limit exceeded")
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if bytes.TrimSpace(line)[0] != '{' {
			return "", errors.New("malformed build record")
		}
		var record struct {
			Error       string          `json:"error"`
			ErrorDetail json.RawMessage `json:"errorDetail"`
			Aux         *struct {
				ID string `json:"ID"`
			} `json:"aux"`
		}
		if json.Unmarshal(line, &record) != nil {
			return "", errors.New("malformed build stream")
		}
		if record.Error != "" || (len(record.ErrorDetail) > 0 && string(record.ErrorDetail) != "null") {
			return "", errors.New("Docker build failed")
		}
		if record.Aux != nil {
			v := record.Aux.ID
			if len(v) != 71 || !strings.HasPrefix(v, "sha256:") {
				return "", errors.New("invalid build identity")
			}
			if _, e := hex.DecodeString(v[7:]); e != nil {
				return "", errors.New("invalid build identity")
			}
			// Classic multistage builds emit an aux ID per stage; the final
			// structured ID must agree with the generated tag after clean EOF.
			image = v
		}
	}
	if scanner.Err() != nil {
		return "", errors.New("build stream incomplete or oversized")
	}
	if image == "" {
		return "", errors.New("build lacks structured result")
	}
	return image, nil
}
func buildTimeout(c Config) time.Duration {
	if c.Build == nil {
		return 0
	}
	d := 10 * time.Minute
	if c.Build.Timeout != "" {
		d, _ = time.ParseDuration(c.Build.Timeout)
	}
	return d
}
