package kubeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"operator-dynamo/internal/contracts"
	"strings"
	"testing"
	"time"
)

func commandObject(o map[string]any) json.RawMessage { return json.RawMessage(encoded(o)) }
func commandDraft(name string) json.RawMessage {
	o, _ := decodeObject([]byte(object(name)))
	o["metadata"].(map[string]any)["namespace"] = "default"
	return commandObject(o)
}
func TestCommandsShareAPIAndAtomicPreconditions(t *testing.T) {
	s := serve(t, []Registration{widget}, DefaultLimits(), nil)
	cmd := contracts.ResourceCommand{SessionID: "test", ActionID: "action", Operation: "create", APIVersion: widget.APIVersion(), Kind: widget.Kind, Namespace: "default", Name: "one", Object: commandDraft("one")}
	r, e := s.Command(context.Background(), cmd)
	if e != nil || r.Status != 201 || r.Disposition != "committed" || r.RequestID == "" {
		t.Fatal(r, e)
	}
	original, _ := decodeObject([]byte(r.ObjectJSON))
	uid, rv := r.UID, r.ResourceVersion
	cmd.Operation = "update"
	cmd.UID = uid
	cmd.ResourceVersion = rv
	cmd.Object = commandObject(original)
	r, e = s.Command(context.Background(), cmd)
	if e != nil || r.Disposition != "no-op" || r.ResourceVersion != rv {
		t.Fatal(r, e)
	}
	for _, field := range []string{"uid", "resourceVersion"} {
		bad := copyObject(original)
		delete(bad["metadata"].(map[string]any), field)
		c := cmd
		c.Object = commandObject(bad)
		r, _ = s.Command(context.Background(), c)
		if r.Status != 422 || r.Disposition != "rejected" {
			t.Fatal(r)
		}
	}
	// One current-RV effective winner; the other cannot overwrite it.
	results := make(chan contracts.CommandResult, 2)
	for _, value := range []string{"first", "second"} {
		go func() {
			c := cmd
			o := copyObject(original)
			o["spec"] = map[string]any{"value": value}
			c.Object = commandObject(o)
			r, _ := s.Command(context.Background(), c)
			results <- r
		}()
	}
	a, b := <-results, <-results
	if !(a.Status == 200 && b.Status == 409 || a.Status == 409 && b.Status == 200) {
		t.Fatal(a, b)
	}
	path := "/apis/example.test/v1/namespaces/default/widgets"
	if c, o := request(t, s, "DELETE", path+"/one", `{}`); c != 200 {
		t.Fatal(c, o)
	}
	if c, o := request(t, s, "POST", path, object("one")); c != 201 {
		t.Fatal(c, o)
	}
	r, _ = s.Command(context.Background(), cmd)
	if r.Status != 409 {
		t.Fatal("recreated name overwritten", r)
	}
	s.CloseCommands()
	r, _ = s.Command(context.Background(), cmd)
	if r.Status != 410 {
		t.Fatal(r)
	}
}
func TestQueuedCommandCancellationDoesNotCommit(t *testing.T) {
	s := serve(t, []Registration{widget}, DefaultLimits(), nil)
	c := contracts.ResourceCommand{Operation: "create", APIVersion: widget.APIVersion(), Kind: widget.Kind, Namespace: "default", Name: "one", Object: commandDraft("one")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.mu.Lock()
	done := make(chan contracts.CommandResult, 1)
	go func() { r, _ := s.Command(ctx, c); done <- r }()
	deadline := time.Now().Add(time.Second)
	for s.requests.Load() == 0 {
		if time.Now().After(deadline) {
			s.mu.Unlock()
			t.Fatal("request did not enter")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	s.mu.Unlock()
	r := <-done
	if r.Status != 408 || r.Disposition != "rejected" {
		t.Fatal(r)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.objects) != 0 || len(s.history) != 0 || s.version != 1 {
		t.Fatal("cancelled commit")
	}
}
func TestCommandKnownCommitFailureAndBudgets(t *testing.T) {
	for _, phase := range []string{"resource.created", "api.request"} {
		t.Run(phase, func(t *testing.T) {
			s := serve(t, []Registration{widget}, DefaultLimits(), func(o contracts.Observation) error {
				if o.Kind == phase {
					return errors.New("failure")
				}
				return nil
			})
			c := contracts.ResourceCommand{Operation: "create", APIVersion: widget.APIVersion(), Kind: widget.Kind, Namespace: "default", Name: "one", Object: commandDraft("one")}
			r, _ := s.Command(context.Background(), c)
			if r.Disposition != "committed-but-session-failed" || r.Status != 503 || r.RequestID == "" {
				t.Fatal(r)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.objects) != 1 || len(s.history) != 1 || s.requests.Load() != 1 {
				t.Fatal("mutation/accounting not shared")
			}
		})
	}
}

func TestCommandNoopJournalFailureIsNotAChange(t *testing.T) {
	fail := false
	s := serve(t, []Registration{widget}, DefaultLimits(), func(o contracts.Observation) error {
		if fail && o.Kind == "api.request" {
			return errors.New("unavailable")
		}
		return nil
	})
	c := contracts.ResourceCommand{Operation: "create", APIVersion: widget.APIVersion(), Kind: widget.Kind, Namespace: "default", Name: "one", Object: commandDraft("one")}
	created, _ := s.Command(context.Background(), c)
	if created.Status != 201 {
		t.Fatal(created)
	}
	c.Operation = "update"
	c.UID = created.UID
	c.ResourceVersion = created.ResourceVersion
	c.Object = json.RawMessage(created.ObjectJSON)
	fail = true
	result, _ := s.Command(context.Background(), c)
	if result.Disposition != "no-op" || !result.SessionFailed || result.Status != 503 || result.ResourceVersion != created.ResourceVersion {
		t.Fatal(result)
	}
}

func TestCommandLimitsAndReadBudgetIsolation(t *testing.T) {
	for _, mode := range []string{"requests", "history", "resources"} {
		t.Run(mode, func(t *testing.T) {
			limits := DefaultLimits()
			switch mode {
			case "requests":
				limits.Requests = 1
			case "history":
				limits.History = 1
			case "resources":
				limits.Resources = 1
			}
			s := serve(t, []Registration{widget}, limits, nil)
			c := contracts.ResourceCommand{Operation: "create", APIVersion: widget.APIVersion(), Kind: widget.Kind, Namespace: "default", Name: "one", Object: commandDraft("one")}
			r, _ := s.Command(context.Background(), c)
			if r.Status != 201 {
				t.Fatal(r)
			}
			for i := 0; i < 10; i++ {
				_, err := s.Inspect(context.Background(), widget.APIVersion(), widget.Kind, "default", "one")
				if err != nil {
					t.Fatal(err)
				}
			}
			if s.requests.Load() != 1 {
				t.Fatal("diagnostic consumed API budget")
			}
			c.Name = "two"
			c.Object = commandDraft("two")
			r, _ = s.Command(context.Background(), c)
			if r.Status != 503 || r.Disposition != "rejected" {
				t.Fatal(r)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.objects) != 1 || len(s.history) != 1 {
				t.Fatal("over-capacity mutated")
			}
		})
	}
}
func TestCommandMalformedAndIdentityNoMutation(t *testing.T) {
	for _, body := range []string{`{broken`, `null`, `[]`, `{"apiVersion":"example.test/v1","kind":"Widget","metadata":{"name":"one"}}`, `{"apiVersion":"example.test/v1","kind":"Widget","metadata":{"name":"different","namespace":"default"}}`} {
		t.Run(body, func(t *testing.T) {
			s := serve(t, []Registration{widget}, DefaultLimits(), nil)
			c := contracts.ResourceCommand{Operation: "create", APIVersion: widget.APIVersion(), Kind: widget.Kind, Namespace: "default", Name: "one", Object: json.RawMessage(body)}
			r, _ := s.Command(context.Background(), c)
			if r.Status < 400 || r.Disposition != "rejected" {
				t.Fatal(r)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.objects) != 0 || len(s.history) != 0 || s.version != 1 {
				t.Fatal("invalid command committed")
			}
		})
	}
}

func TestCommandLockWaitHonorsDeadline(t *testing.T) {
	s := serve(t, []Registration{widget}, DefaultLimits(), nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan contracts.CommandResult, 1)
	go func() {
		r, _ := s.Command(ctx, contracts.ResourceCommand{Operation: "create", APIVersion: widget.APIVersion(), Kind: widget.Kind, Namespace: "default", Name: "one", Object: commandDraft("one")})
		done <- r
	}()
	select {
	case r := <-done:
		if r.Status != 408 || r.Disposition != "rejected" {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("command blocked beyond deadline")
	}
	if len(s.objects) != 0 || len(s.history) != 0 {
		t.Fatal("cancelled commit")
	}
}

func TestCommandEscapedResponseStillReportsKnownCommit(t *testing.T) {
	s := serve(t, []Registration{widget}, DefaultLimits(), nil)
	obj, _ := decodeObject(commandDraft("one"))
	obj["spec"] = map[string]any{"text": strings.Repeat("<", 400000)}
	var raw bytes.Buffer
	e := json.NewEncoder(&raw)
	e.SetEscapeHTML(false)
	if err := e.Encode(obj); err != nil {
		t.Fatal(err)
	}
	c := contracts.ResourceCommand{Operation: "create", APIVersion: widget.APIVersion(), Kind: widget.Kind, Namespace: "default", Name: "one", Object: raw.Bytes()}
	r, _ := s.Command(context.Background(), c)
	if r.Status != 201 || r.Disposition != "committed" || r.UID == "" || len(r.ObjectJSON) < 2<<20 {
		t.Fatal(r.Status, r.Disposition, len(r.ObjectJSON))
	}
}
