package scenario

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"operator-dynamo/internal/contracts"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type planner struct {
	ctx            context.Context
	source         Source
	resources      []Resource
	plan           Plan
	files          map[string][]byte
	fixtures       map[string]map[string]any
	seen           map[string]bool
	dirs           map[string]bool
	visited, total int
}

func contained(p string) bool {
	return p != "" && p != ".." && !strings.HasPrefix(p, "../") && !strings.HasPrefix(p, "/") && p != "sessions" && p != "runs" && !strings.HasPrefix(p, "sessions/") && !strings.HasPrefix(p, "runs/") && utf8.ValidString(p) && !strings.ContainsRune(p, 0)
}
func reference(base, value string) (string, bool) {
	if value == "" || strings.HasPrefix(value, "/") {
		return "", false
	}
	v := path.Clean(path.Join(path.Dir(base), value))
	return v, contained(v)
}
func BuildPlan(ctx context.Context, source Source, selection string, resources []Resource) (*Plan, error) {
	selection = path.Clean(selection)
	if !contained(selection) {
		return nil, invalid(selection)
	}
	p := &planner{ctx: ctx, source: source, resources: resources, plan: Plan{Selection: selection}, files: map[string][]byte{}, fixtures: map[string]map[string]any{}, seen: map[string]bool{}, dirs: map[string]bool{}}
	if e := p.selectPath(selection, nil, 0, false); e != nil {
		return nil, e
	}
	if len(p.plan.Scenarios) == 0 {
		return nil, invalid(selection)
	}
	for name, b := range p.files {
		h := sha256.Sum256(b)
		p.plan.Inputs = append(p.plan.Inputs, contracts.InputDigest{Path: name, SHA256: hex.EncodeToString(h[:])})
	}
	sort.Slice(p.plan.Inputs, func(i, j int) bool { return p.plan.Inputs[i].Path < p.plan.Inputs[j].Path })
	return &p.plan, nil
}
func (p *planner) read(name string) ([]byte, error) {
	if strings.Count(name, "/") > 64 {
		return nil, invalid(name)
	}
	if b, ok := p.files[name]; ok {
		return b, nil
	}
	v, e := p.source.ReadSource(p.ctx, name, 1<<20)
	if e != nil {
		return nil, p.inputError(name, e)
	}
	p.total += len(v.Data)
	if p.total > 16<<20 {
		return nil, invalid(name)
	}
	p.files[name] = v.Data
	return v.Data, nil
}
func (p *planner) inputError(name string, e error) error {
	if errors.Is(e, os.ErrNotExist) || errors.Is(e, contracts.ErrInvalid) {
		return invalid(name)
	}
	return &Error{Reason: "input_failed", Path: name}
}
func (p *planner) selectPath(name string, ancestry []string, level int, explicit bool) error {
	if e := p.ctx.Err(); e != nil {
		return e
	}
	if !contained(name) || level > 32 || strings.Count(name, "/") > 64 {
		return invalid(name)
	}
	entry, e := p.source.Inspect(name)
	if e != nil {
		return p.inputError(name, e)
	}
	if entry.Directory {
		if name != "." && strings.Count(name, "/")+1 > 64 {
			return invalid(name)
		}
		if p.dirs[name] {
			return invalid(name)
		}
		p.dirs[name] = true
		for _, preferred := range []string{"suite.yaml", "scenario.yaml"} {
			selected := path.Join(name, preferred)
			entry, err := p.source.Inspect(selected)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return p.inputError(selected, err)
			}
			if !entry.Regular {
				return invalid(selected)
			}
			return p.selectPath(selected, ancestry, level, true)
		}
		entries, token, e := p.source.ListDirectory(p.ctx, name, 10000-p.visited)
		if e != nil {
			return p.inputError(name, e)
		}
		p.visited += len(entries)
		before := len(p.plan.Scenarios)
		found := ""
		for _, preferred := range []string{"suite.yaml", "scenario.yaml"} {
			for _, entry := range entries {
				if !utf8.ValidString(entry.Name) {
					return invalid(name)
				}
				if entry.Name == preferred {
					if !entry.Regular {
						return invalid(name)
					}
					found = preferred
					break
				}
			}
			if found != "" {
				break
			}
		}
		if found != "" {
			if e = p.selectPath(path.Join(name, found), ancestry, level, true); e != nil {
				return e
			}
		} else {
			for _, entry := range entries {
				if entry.Directory {
					if e = p.selectPath(path.Join(name, entry.Name), ancestry, level, false); e != nil {
						return e
					}
				}
			}
		}
		if e = p.source.CheckDirectory(name, token); e != nil {
			return p.inputError(name, e)
		}
		if explicit && len(p.plan.Scenarios) == before {
			return invalid(name)
		}
		return nil
	}
	if !entry.Regular || p.seen[name] {
		return invalid(name)
	}
	p.seen[name] = true
	b, e := p.read(name)
	if e != nil {
		return e
	}
	m, e := parse(b, name)
	if e != nil {
		return e
	}
	if str(m["apiVersion"]) != "dynamo/v1alpha1" || !identifier.MatchString(str(m["name"])) {
		return invalid(name)
	}
	switch str(m["kind"]) {
	case "Suite":
		if level >= 32 || !keys(m, "apiVersion", "kind", "name", "entries") {
			return invalid(name)
		}
		entries, ok := m["entries"].([]any)
		if !ok || len(entries) == 0 {
			return invalid(name)
		}
		for _, value := range entries {
			child, ok := reference(name, str(value))
			if !ok {
				return invalid(name)
			}
			chain := append(append([]string(nil), ancestry...), name)
			if e := p.selectPath(child, chain, level+1, true); e != nil {
				return e
			}
		}
		return nil
	case "Scenario":
		if len(p.plan.Scenarios) >= 100 {
			return invalid(name)
		}
		s, e := p.scenario(m, name, ancestry)
		if e != nil {
			return e
		}
		p.plan.Scenarios = append(p.plan.Scenarios, s)
		return nil
	}
	return invalid(name)
}
func (p *planner) target(m map[string]any, exact bool) (Target, bool) {
	t := Target{}
	if m == nil || !keys(m, "apiVersion", "kind", "namespace", "name", "matchLabels") {
		return t, false
	}
	t.APIVersion = str(m["apiVersion"])
	t.Kind = str(m["kind"])
	t.Namespace = str(m["namespace"])
	t.Name = str(m["name"])
	var found bool
	for _, r := range p.resources {
		if r.APIVersion == t.APIVersion && r.Kind == t.Kind {
			t.Resource = r
			found = true
			break
		}
	}
	if !found {
		return t, false
	}
	_, namespaceSet := m["namespace"]
	if t.Resource.Scope == "Namespaced" {
		if t.Namespace == "" {
			return t, false
		}
	} else if namespaceSet {
		return t, false
	}
	_, nameSet := m["name"]
	labels, labelsSet := m["matchLabels"]
	if nameSet == labelsSet || exact && !nameSet || nameSet && t.Name == "" {
		return t, false
	}
	if labelsSet {
		v, ok := labels.(map[string]any)
		if !ok {
			return t, false
		}
		t.MatchLabels = map[string]string{}
		for k, v := range v {
			s, ok := v.(string)
			if !ok {
				return t, false
			}
			t.MatchLabels[k] = s
		}
	}
	// URL path segments cannot create additional routes.
	for _, v := range []string{t.Name, t.Namespace} {
		if strings.ContainsAny(v, "/\\\x00") || v == "." || v == ".." {
			return t, false
		}
	}
	return t, true
}
func (p *planner) scenario(m map[string]any, name string, ancestry []string) (Scenario, error) {
	s := Scenario{SourcePath: name, Name: str(m["name"]), SuitePaths: append([]string(nil), ancestry...)}
	bad := func() (Scenario, error) { return s, invalid(name) }
	if !keys(m, "apiVersion", "kind", "name", "timeout", "steps") {
		return bad()
	}
	var ok bool
	s.Timeout, ok = durationField(m, "timeout", 60*time.Second, 100*time.Millisecond, 10*time.Minute)
	if !ok {
		return bad()
	}
	steps, ok := m["steps"].([]any)
	if !ok || len(steps) == 0 || len(steps) > 100 {
		return bad()
	}
	ids, observations := map[string]bool{}, map[string]bool{}
	for _, raw := range steps {
		sm := object(raw)
		if sm == nil || !keys(sm, "id", "create", "update", "delete", "assert") {
			return bad()
		}
		step := Step{ID: str(sm["id"])}
		if !identifier.MatchString(step.ID) || ids[step.ID] {
			return bad()
		}
		ids[step.ID] = true
		for _, op := range []string{"create", "update", "delete", "assert"} {
			if _, has := sm[op]; has {
				if step.Operation != "" {
					return bad()
				}
				step.Operation = op
			}
		}
		v := object(sm[step.Operation])
		if step.Operation == "" || v == nil {
			return bad()
		}
		switch step.Operation {
		case "create", "update":
			if !keys(v, "fixture") {
				return bad()
			}
			fp, ok := reference(name, str(v["fixture"]))
			if !ok {
				return bad()
			}
			b, e := p.read(fp)
			if e != nil {
				return s, e
			}
			fixture, exists := p.fixtures[fp]
			if !exists {
				fixture, e = parse(b, fp)
				if e != nil {
					return s, e
				}
				p.fixtures[fp] = fixture
			}
			meta := object(fixture["metadata"])
			if meta == nil {
				return bad()
			}
			tm := map[string]any{"apiVersion": fixture["apiVersion"], "kind": fixture["kind"], "name": meta["name"]}
			if n, has := meta["namespace"]; has {
				tm["namespace"] = n
			}
			step.Target, ok = p.target(tm, true)
			if !ok {
				return bad()
			}
			if _, has := meta["generateName"]; has {
				return bad()
			}
			if step.Operation == "update" {
				for _, key := range []string{"resourceVersion", "uid", "creationTimestamp", "generation"} {
					if _, has := meta[key]; has {
						return bad()
					}
				}
				if _, has := fixture["status"]; has {
					return bad()
				}
			}
			step.Fixture = fixture
			step.FixturePath = fp
		case "delete":
			if !keys(v, "target") {
				return bad()
			}
			step.Target, ok = p.target(object(v["target"]), true)
			if !ok {
				return bad()
			}
		case "assert":
			if !keys(v, "target", "count", "fields", "saveAs", "timeout") {
				return bad()
			}
			a := &Assertion{}
			a.Target, ok = p.target(object(v["target"]), false)
			if !ok {
				return bad()
			}
			n, ok := v["count"].(json.Number)
			if !ok {
				return bad()
			}
			count, e := strconv.Atoi(string(n))
			if e != nil || count < 0 || count > 256 || a.Target.Name != "" && count > 1 {
				return bad()
			}
			a.Count = count
			a.Timeout, ok = durationField(v, "timeout", 10*time.Second, 100*time.Millisecond, 5*time.Minute)
			if !ok {
				return bad()
			}
			if raw, has := v["fields"]; has {
				fields, ok := raw.([]any)
				if !ok || len(fields) > 64 || count != 1 {
					return bad()
				}
				for _, raw := range fields {
					f := object(raw)
					if f == nil || !keys(f, "pointer", "equals", "equalsFrom", "exists") {
						return bad()
					}
					pointer, ok := f["pointer"].(string)
					if !ok || !validPointer(pointer) {
						return bad()
					}
					field := Field{Pointer: pointer}
					choices := 0
					if x, has := f["equals"]; has {
						choices++
						field.Equals = x
						field.EqualsSet = true
					}
					if x, has := f["exists"]; has {
						b, ok := x.(bool)
						if !ok {
							return bad()
						}
						choices++
						field.Exists = &b
					}
					if x, has := f["equalsFrom"]; has {
						r := object(x)
						if r == nil || !keys(r, "observation", "pointer") {
							return bad()
						}
						pointer, ok := r["pointer"].(string)
						if !ok || !validPointer(pointer) || !observations[str(r["observation"])] {
							return bad()
						}
						choices++
						field.EqualsFrom = &Reference{Observation: str(r["observation"]), Pointer: pointer}
					}
					if choices != 1 {
						return bad()
					}
					a.Fields = append(a.Fields, field)
				}
			}
			if value, has := v["saveAs"]; has {
				a.SaveAs = str(value)
				if count != 1 || !identifier.MatchString(a.SaveAs) || observations[a.SaveAs] || len(observations) >= 32 {
					return bad()
				}
				observations[a.SaveAs] = true
			}
			step.Assert = a
			step.Target = a.Target
		}
		s.Steps = append(s.Steps, step)
	}
	return s, nil
}
