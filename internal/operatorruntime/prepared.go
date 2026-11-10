package operatorruntime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"operator-dynamo/internal/contracts"
	"path"
	"sort"
	"strings"
)

// Prepared owns immutable input bytes. Each runtime clones mutable evidence, not
// the archived bytes; neither the caller nor a later session can modify them.
type Prepared struct {
	archive  []byte
	files    []contracts.InputFile
	evidence *buildEvidence
	outputs  map[string][]byte
	manifest []contracts.InputDigest
}
type preparationInputs struct {
	Inputs
	outputs map[string][]byte
	hashes  map[string]string
}

func (p *preparationInputs) ReadSource(ctx context.Context, name string, n int64) (contracts.InputFile, error) {
	v, e := p.Inputs.ReadSource(ctx, name, n)
	if e == nil {
		if old, ok := p.hashes[name]; ok && old != digest(v.Data) {
			return contracts.InputFile{}, contracts.ErrInvalid
		}
		p.hashes[name] = digest(v.Data)
	}
	return v, e
}
func (p *preparationInputs) Snapshot(ctx context.Context, name string, include func(string, bool) (bool, error)) ([]contracts.InputFile, error) {
	v, e := p.Inputs.Snapshot(ctx, name, include)
	if e == nil {
		for _, f := range v {
			key := path.Join(name, f.Name)
			if old, ok := p.hashes[key]; ok && old != digest(f.Data) {
				return nil, contracts.ErrInvalid
			}
			p.hashes[key] = digest(f.Data)
		}
	}
	return v, e
}
func (p *preparationInputs) WriteOutput(ctx context.Context, name string, b []byte) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	p.outputs[name] = append([]byte(nil), b...)
	return nil
}
func Prepare(ctx context.Context, cfg Config, inputs Inputs) (*Prepared, error) {
	if e := cfg.Validate(); e != nil {
		return nil, e
	}
	p := &preparationInputs{Inputs: inputs, outputs: map[string][]byte{}, hashes: map[string]string{}}
	r := &Runtime{Config: cfg, Inputs: p}
	b, e := r.prepareInputs(ctx)
	if e != nil {
		return nil, e
	}
	out := &Prepared{archive: b, files: r.inputFiles, evidence: r.buildInfo, outputs: p.outputs}
	for k, v := range p.hashes {
		out.manifest = append(out.manifest, contracts.InputDigest{Path: k, SHA256: v})
	}
	sort.Slice(out.manifest, func(i, j int) bool { return out.manifest[i].Path < out.manifest[j].Path })
	return out, nil
}
func (p *Prepared) Manifest() []contracts.InputDigest {
	return append([]contracts.InputDigest(nil), p.manifest...)
}
func (r *Runtime) preparedInputs(ctx context.Context) ([]byte, error) {
	if r.Prepared == nil {
		return r.prepareInputs(ctx)
	}
	p := r.Prepared
	r.inputFiles = append([]contracts.InputFile(nil), p.files...)
	if p.evidence != nil {
		b, _ := json.Marshal(p.evidence)
		if e := json.Unmarshal(b, &r.buildInfo); e != nil {
			return nil, e
		}
	}
	for name, b := range p.outputs {
		if e := r.Inputs.WriteOutput(ctx, name, b); e != nil {
			return nil, e
		}
	}
	return p.archive, nil
}
func PinImage(ctx context.Context, engine Engine, reference string) (string, error) {
	var image struct {
		ID string `json:"Id"`
	}
	if e := engine.Call(ctx, "GET", "/images/"+url.PathEscape(reference)+"/json", nil, &image); e != nil {
		return "", e
	}
	if len(image.ID) != 71 || !strings.HasPrefix(image.ID, "sha256:") {
		return "", contracts.ErrInvalid
	}
	if _, e := hex.DecodeString(image.ID[7:]); e != nil {
		return "", contracts.ErrInvalid
	}
	return image.ID, nil
}
