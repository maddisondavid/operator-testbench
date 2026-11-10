package operatorruntime

import (
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"strings"
	"testing"
)

func TestLiteralControlWhitespace(t *testing.T) {
	for _, name := range []string{"#Dockerfile", "!Dockerfile", " Dockerfile", "Dockerfile ", "\tDockerfile\t", "nested/Probe[1].Dockerfile"} {
		rules, e := ignorefile.ReadAll(strings.NewReader(literalPattern(name) + "\n"))
		if e != nil {
			t.Fatal(e)
		}
		matcher, e := patternmatcher.New(rules)
		if e != nil {
			t.Fatal(e)
		}
		matched, e := matcher.MatchesOrParentMatches(name)
		if e != nil || !matched {
			t.Fatalf("literal control exclusion failed for %q: %v", name, e)
		}
	}
}
