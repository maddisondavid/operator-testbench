// Package cli translates process arguments and application outcomes. It does not
// depend on the concrete application or instantiate domain components.
package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/uiaddress"
)

type Runner interface {
	Run(context.Context, contracts.Options) error
}

func Execute(ctx context.Context, args []string, output io.Writer, runner Runner) int {
	// Detect an attempted verbose selection before FlagSet can write anything.
	attempted := false
	for _, arg := range args {
		if arg == "--ui-write" || arg == "-ui-write" || strings.HasPrefix(arg, "--ui-write=") || strings.HasPrefix(arg, "-ui-write=") || arg == "--ui" || arg == "-ui" || strings.HasPrefix(arg, "--ui=") || strings.HasPrefix(arg, "-ui=") || arg == "--ui-origin" || strings.HasPrefix(arg, "--ui-origin=") || arg == "-ui-origin" || strings.HasPrefix(arg, "-ui-origin=") || arg == "--verbose" || arg == "-verbose" || strings.HasPrefix(arg, "--verbose=") || strings.HasPrefix(arg, "-verbose=") {
			attempted = true
		}
	}
	var console *consoleOutput
	if attempted {
		sink, release, e := admit(output)
		if e != nil {
			return 2
		}
		console = newOutput(sink, release)
	}
	var parsed cappedBuffer
	parseOutput := output
	if attempted {
		parseOutput = &parsed
	}
	finishEarly := func(code int) int {
		if console != nil {
			c, stop := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer stop()
			console.Finish(c, func(w io.Writer) { w.Write(parsed.Bytes()) })
		}
		return code
	}
	flags := flag.NewFlagSet("dynamo", flag.ContinueOnError)
	flags.SetOutput(parseOutput)
	var o contracts.Options
	var diagnostics []contracts.APIDiagnostic
	o.Diagnose = func(d contracts.APIDiagnostic) {
		if len(diagnostics) < 100 {
			diagnostics = append(diagnostics, d)
		}
	}
	render := func(w io.Writer, err error, tests bool) int {
		for _, d := range diagnostics {
			terminal := d.Terminal
			if terminal == "" {
				terminal = "no terminal API failure recorded"
			}
			fmt.Fprintf(w, "API diagnostics session=%s: %s; summary capture=%s; cleanup=%s\n", d.SessionID, terminal, d.Capture, d.Cleanup)
			fmt.Fprint(w, d.Notice)
		}
		return renderResult(w, err, tests)
	}
	flags.BoolVar(&o.Verbose, "verbose", false, "show bounded lifecycle progress")
	flags.StringVar(&o.Workdir, "workdir", "", "required workspace directory")
	flags.StringVar(&o.Tests, "tests", "", "contained scenario or suite path")
	flags.BoolVar(&o.UI, "ui", false, "enable read-only workbench")
	flags.BoolVar(&o.UIWrite, "ui-write", false, "enable interactive non-Secret JSON create/update (requires --ui and --ui-origin)")
	flags.StringVar(&o.UIOrigin, "ui-origin", "", "explicit trusted browser-facing http://IP:PORT")
	flags.StringVar(&o.Listen, "listen", "127.0.0.1:8080", "HTTP listen address")
	flags.Usage = func() {
		fmt.Fprintln(parseOutput, "Usage: dynamo --workdir DIR [--listen HOST:PORT] [--tests PATH] [--verbose] [--ui]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return finishEarly(0)
		}
		if attempted {
			parsed.Reset()
			fmt.Fprintln(&parsed, "invalid command arguments")
		}
		return finishEarly(2)
	}
	invalid := func(message string) int { fmt.Fprintln(parseOutput, message); flags.Usage(); return finishEarly(2) }
	if flags.NArg() != 0 {
		return invalid("unexpected positional arguments")
	}
	if strings.TrimSpace(o.Workdir) == "" {
		return invalid("--workdir is required and must be nonempty")
	}
	host, port, err := net.SplitHostPort(o.Listen)
	_ = host
	if err != nil {
		return invalid("--listen must be HOST:PORT")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return invalid("--listen port must be an integer between 0 and 65535")
	}
	testsSet := false
	originSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "ui-origin" {
			originSet = true
		}
		if f.Name == "tests" {
			testsSet = true
		}
	})
	if testsSet && strings.TrimSpace(o.Tests) == "" {
		return invalid("--tests must be nonempty")
	}
	if originSet {
		if !o.UI {
			return invalid("--ui-origin requires --ui")
		}
		if _, e := uiaddress.Origin(o.UIOrigin); e != nil {
			return invalid("invalid --ui-origin")
		}
	}
	if o.UIWrite && (!o.UI || !originSet || testsSet) {
		return invalid("--ui-write requires --ui, explicit --ui-origin, and no --tests")
	}
	if console != nil && !o.Verbose && !o.UI {
		finishEarly(0)
		console = nil
	}
	if console != nil {
		console.enqueue("Operator Dynamo\n", false)
		o.Console = &contracts.Console{Observe: func(p contracts.Progress) {
			if o.Verbose || p.Kind == contracts.UIAvailable {
				console.Observe(p)
			}
		}, Finish: func(ctx context.Context, err error) {
			console.Finish(ctx, func(w io.Writer) { render(w, err, testsSet) })
		}}
	} else {
		fmt.Fprintln(output, "Operator Dynamo")
	}
	err = runner.Run(ctx, o)
	if console != nil {
		if o.Verbose && resultCode(err) != 0 {
			kind := contracts.PreparationFailed
			var r *contracts.TestResult
			if errors.As(err, &r) && r.Status == "cancelled" {
				kind = contracts.Cancelled
			}
			console.Observe(contracts.Progress{Kind: kind})
		}
		// Only early-return paths lacking an application cleanup context reach this fallback.
		c, stop := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer stop()
		console.Finish(c, func(w io.Writer) { render(w, err, testsSet) })
		return resultCode(err)
	}
	return render(output, err, testsSet)
}
func resultCode(err error) int {
	if err == nil {
		return 0
	}
	var r *contracts.TestResult
	if errors.As(err, &r) {
		return r.Code
	}
	if errors.Is(err, contracts.ErrInvalid) {
		return 2
	}
	return 3
}
func renderResult(output io.Writer, err error, testsSet bool) int {

	if err != nil {
		var testResult *contracts.TestResult
		if errors.As(err, &testResult) {
			for _, s := range testResult.Scenarios {
				fmt.Fprintf(output, "%s %s step=%s reason=%s\n", s.Status, s.Name, s.Step, s.Reason)
			}
			fmt.Fprintf(output, "tests: %s passed=%d failed=%d error=%d cancelled=%d skipped=%d report=%q reason=%s\n", testResult.Status, testResult.Counts["passed"], testResult.Counts["failed"], testResult.Counts["error"], testResult.Counts["cancelled"], testResult.Counts["skipped"], testResult.ReportPath, testResult.Reason)
			if testResult.Code == 3 && testResult.Reason == "report_failed" {
				fmt.Fprintln(output, "terminal report could not be finalized; this run is unsuccessful")
			}
			return testResult.Code
		}
		if testsSet {
			fmt.Fprintln(output, "dynamo: test infrastructure failed")
			if errors.Is(err, contracts.ErrInvalid) {
				return 2
			}
			return 3
		}

		if errors.Is(err, contracts.ErrInvalid) {
			fmt.Fprintln(output, "dynamo: invalid configuration")
		} else {
			fmt.Fprintln(output, "dynamo: infrastructure failed")
		}
		if errors.Is(err, contracts.ErrInvalid) {
			return 2
		}
		return 3
	}
	return 0
}

// Parser diagnostics never retain unbounded argv text or reach an unchecked sink.
type cappedBuffer struct{ buffer bytes.Buffer }

func (b *cappedBuffer) Bytes() []byte { return b.buffer.Bytes() }
func (b *cappedBuffer) Reset()        { b.buffer.Reset() }
func (b *cappedBuffer) Len() int      { return b.buffer.Len() }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := (64 << 10) - b.Len()
	if left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		b.buffer.Write(p)
	}
	return n, nil
}
