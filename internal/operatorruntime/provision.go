package operatorruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"operator-dynamo/internal/contracts"
	"os"
	"path"
	"sort"
	"strings"
)

type PathInspector interface {
	CheckPaths(context.Context, string, []string) error
}

func (d *Docker) CheckPaths(ctx context.Context, id string, paths []string) error {
	seen := map[string]bool{}
	for _, target := range paths {
		for p := target; p != "/" && p != "."; p = path.Dir(p) {
			if seen[p] {
				continue
			}
			seen[p] = true
			req, _ := http.NewRequestWithContext(ctx, "HEAD", "http://docker/v1.47/containers/"+id+"/archive?path="+url.QueryEscape(p), nil)
			res, e := d.client.Do(req)
			if e != nil {
				return errors.New("image path inspection failed")
			}
			res.Body.Close()
			if res.StatusCode == 404 {
				continue
			}
			if res.StatusCode != 200 {
				return errors.New("image path inspection failed")
			}
			raw, e := base64.StdEncoding.DecodeString(res.Header.Get("X-Docker-Container-Path-Stat"))
			if e != nil {
				return errors.New("invalid image path metadata")
			}
			var info struct {
				Mode       uint32
				LinkTarget string
			}
			if json.Unmarshal(raw, &info) != nil {
				return errors.New("invalid image path metadata")
			}
			if os.FileMode(info.Mode)&os.ModeSymlink != 0 || info.LinkTarget != "" {
				return errors.New("unsafe image mount path")
			}
		}
	}
	return nil
}
func (r *Runtime) labels() map[string]string {
	return map[string]string{"io.dynamo.session": r.session, "io.dynamo.owner": r.owner}
}
func (r *Runtime) owns(ctx context.Context, kind, id string) (bool, error) {
	var info struct {
		Labels map[string]string
		Config struct{ Labels map[string]string }
	}
	suffix := ""
	if kind == "containers" {
		suffix = "/json"
	}
	e := r.Engine.Call(ctx, "GET", "/"+kind+"/"+url.PathEscape(id)+suffix, nil, &info)
	if IsNotFound(e) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	labels := info.Labels
	if kind == "containers" {
		labels = info.Config.Labels
	}
	return labels["io.dynamo.owner"] == r.owner && labels["io.dynamo.session"] == r.session, nil
}
func (r *Runtime) createVolume(ctx context.Context, name string) error {
	r.volumes = append(r.volumes, name)
	if e := r.Engine.Call(ctx, "POST", "/volumes/create", jsonBody(map[string]any{"Name": name, "Labels": r.labels()}), nil); e != nil {
		return e
	}
	own, e := r.owns(ctx, "volumes", name)
	if e != nil {
		return e
	}
	if !own {
		return errors.New("volume ownership collision")
	}
	return nil
}
func (r *Runtime) createContainer(ctx context.Context, name string, body map[string]any) (string, error) {
	body["Labels"] = r.labels()
	r.containers = append(r.containers, name)
	var created struct {
		ID string `json:"Id"`
	}
	e := r.Engine.Call(ctx, "POST", "/containers/create?name="+url.QueryEscape(name), jsonBody(body), &created)
	if e != nil {
		return "", e
	}
	id := created.ID
	if id == "" {
		return "", errors.New("missing created container identity")
	}
	own, e := r.owns(ctx, "containers", id)
	if e != nil {
		return "", e
	}
	if !own {
		return "", errors.New("container ownership mismatch")
	}
	r.containers[len(r.containers)-1] = id
	return id, nil
}
func (r *Runtime) removeContainer(ctx context.Context, id string) error {
	own, e := r.owns(ctx, "containers", id)
	if e != nil {
		return e
	}
	if !own {
		return nil
	}
	e = r.Engine.Call(ctx, "DELETE", "/containers/"+url.PathEscape(id)+"?force=true&v=true", nil, nil)
	if IsNotFound(e) {
		return nil
	}
	return e
}
func mount(name, target string, ro bool) map[string]any {
	return map[string]any{"Type": "volume", "Source": name, "Target": target, "ReadOnly": ro, "VolumeOptions": map[string]any{"NoCopy": true}}
}
func archiveFiles(files []contracts.InputFile, rootMode int64, rootName string) ([]byte, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	dirs := map[string]bool{".": true}
	for _, f := range files {
		for p := path.Dir(f.Name); p != "."; p = path.Dir(p) {
			dirs[p] = true
		}
	}
	names := make([]string, 0, len(dirs))
	for p := range dirs {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		if e := tw.WriteHeader(&tar.Header{Name: path.Join(rootName, p) + "/", Typeflag: tar.TypeDir, Mode: rootMode}); e != nil {
			return nil, e
		}
	}
	for _, f := range files {
		if e := tw.WriteHeader(&tar.Header{Name: path.Join(rootName, f.Name), Mode: 0444, Size: int64(len(f.Data))}); e != nil {
			return nil, e
		}
		if _, e := tw.Write(f.Data); e != nil {
			return nil, e
		}
	}
	if e := tw.Close(); e != nil {
		return nil, e
	}
	return b.Bytes(), nil
}
func (r *Runtime) provision(ctx context.Context) (map[string]any, error) {
	targets := []string{"/dynamo-config"}
	if len(r.inputFiles) > 0 {
		targets = append(targets, "/dynamo-inputs")
	}
	for _, v := range r.Config.Volumes {
		targets = append(targets, "/dynamo-work/"+v.Name)
	}
	checker, ok := r.Engine.(PathInspector)
	if !ok {
		return nil, errors.New("Docker image path inspection unavailable")
	}
	helperName := "dynamo-prepare-" + strings.ToLower(r.session)
	probe, e := r.createContainer(ctx, helperName, map[string]any{"Image": r.image, "HostConfig": map[string]any{"LogConfig": map[string]string{"Type": "none"}}})
	if e != nil {
		return nil, e
	}
	if e = checker.CheckPaths(ctx, probe, targets); e != nil {
		return nil, e
	}
	if e = r.removeContainer(ctx, probe); e != nil {
		return nil, e
	}
	config := map[string]any{"apiVersion": "v1", "kind": "Config", "clusters": []any{map[string]any{"name": "dynamo", "cluster": map[string]string{"server": r.Endpoint}}}, "contexts": []any{map[string]any{"name": "dynamo", "context": map[string]string{"cluster": "dynamo"}}}, "current-context": "dynamo", "users": []any{}}
	content, _ := json.Marshal(config)
	groups := [][]contracts.InputFile{{{Name: "kubeconfig", Data: content}}}
	modes := []int64{0555}
	if len(r.inputFiles) > 0 {
		groups = append(groups, r.inputFiles)
		modes = append(modes, 0555)
	}
	for range r.Config.Volumes {
		groups = append(groups, nil)
		modes = append(modes, 0777)
	}
	var helperMounts, operatorMounts []any
	for i, target := range targets {
		name := fmt.Sprintf("dynamo-data-%s-%d", strings.ToLower(r.session), i)
		if e = r.createVolume(ctx, name); e != nil {
			return nil, e
		}
		helperMounts = append(helperMounts, mount(name, target, false))
		operatorMounts = append(operatorMounts, mount(name, target, i < 1+lenBool(len(r.inputFiles) > 0)))
	}
	helper, e := r.createContainer(ctx, helperName, map[string]any{"Image": r.image, "HostConfig": map[string]any{"Mounts": helperMounts, "LogConfig": map[string]string{"Type": "none"}}})
	if e != nil {
		return nil, e
	}
	for i, target := range targets {
		data, e := archiveFiles(groups[i], modes[i], path.Base(target))
		if e != nil {
			return nil, e
		}
		if e = r.Engine.Call(ctx, "PUT", "/containers/"+helper+"/archive?path="+url.QueryEscape(path.Dir(target)), bytes.NewReader(data), nil); e != nil {
			return nil, e
		}
	}
	if e = r.removeContainer(ctx, helper); e != nil {
		return nil, e
	}
	env := []string{"KUBECONFIG=/dynamo-config/kubeconfig"}
	keys := make([]string, 0, len(r.Config.Env))
	for k := range r.Config.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+r.Config.Env[k])
	}
	body := map[string]any{"Image": r.image, "Env": env, "AttachStdout": r.Config.Logs.Mode == "capture", "AttachStderr": r.Config.Logs.Mode == "capture", "Tty": false, "HostConfig": map[string]any{"ReadonlyRootfs": true, "Mounts": operatorMounts, "LogConfig": map[string]string{"Type": "none"}}}
	if r.Config.Args != nil {
		body["Cmd"] = r.Config.Args
	}
	return body, nil
}
func lenBool(v bool) int {
	if v {
		return 1
	}
	return 0
}
