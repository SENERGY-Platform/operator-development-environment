package experiments_test

import (
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// The parser reads a file from a repository ODE does not own, so it has to
// terminate and refuse rather than hang, panic or half-read.
func FuzzParseCriteria(f *testing.F) {
	f.Add("metric: rmse\nthreshold: 0.3\n")
	f.Add("criteria:\n  - metric: a\n    threshold: 1\n")
	f.Add("a: |\n  b\n  c\n")
	f.Add("- - -\n")
	f.Add(strings.Repeat("a:\n ", 50))
	f.Add("[\n")
	f.Add("\"\n")
	f.Add("---\n---\n")
	f.Fuzz(func(t *testing.T, source string) {
		document, err := experiments.ParseCriteria(source)
		if err != nil {
			return
		}
		// The invariant that can fail: a criterion is only ever read out of a key that
		// names one. Without it the reader guessed a metric from `name:` and a
		// threshold from `value:`, and produced a criterion the developer never wrote
		// — which then displaced the run's own evaluation tags.
		if document.Primary != nil {
			if strings.TrimSpace(document.Primary.Metric) == "" {
				t.Fatalf("a criterion with no metric was accepted from %q", source)
			}
			if !namesACriterion(source) {
				t.Fatalf("a criterion (%q) was read out of %q, which names none",
					document.Primary.Metric, source)
			}
		}
		for _, spec := range document.Secondary {
			if strings.TrimSpace(spec.Metric) == "" {
				t.Fatalf("a secondary criterion with no metric was accepted from %q", source)
			}
		}
	})
}

// namesACriterion is the crude, independent check the fuzzer grades the parser
// against: does the text contain any key under which a metric may be written at
// all. Deliberately not the parser's own logic — an invariant expressed in terms of
// the implementation would agree with any bug it has.
func namesACriterion(source string) bool {
	for _, key := range []string{
		"metric", "primary_metric", "target_metric", "criteria", "criterion", "name",
	} {
		if strings.Contains(source, key) {
			return true
		}
	}
	return false
}
