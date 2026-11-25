package appapi

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"
)

// api/app.openapi.yaml is the app contract. This test fails when a
// route exists in Go but not in the spec, or vice versa.
func TestOpenAPISpecMatchesRoutes(t *testing.T) {
	f, err := os.Open("../../api/app.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	pathRe := regexp.MustCompile(`^  (/[^:]*):\s*$`)
	methodRe := regexp.MustCompile(`^    (get|post|put|patch|delete):`)
	spec := map[string]bool{}
	var cur string
	sc := bufio.NewScanner(f)
	inPaths := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && line != "" && !strings.HasPrefix(line, " "):
			inPaths = false
		case inPaths && pathRe.MatchString(line):
			cur = pathRe.FindStringSubmatch(line)[1]
		case inPaths && cur != "" && methodRe.MatchString(line):
			spec[strings.ToUpper(methodRe.FindStringSubmatch(line)[1])+" "+cur] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(spec) == 0 {
		t.Fatal("parsed no paths from the spec")
	}
	code := map[string]bool{}
	for _, r := range Routes(Deps{}) {
		code[r.Method+" "+r.Pattern] = true
	}
	for k := range code {
		if !spec[k] {
			t.Errorf("route %s is not documented in api/app.openapi.yaml", k)
		}
	}
	for k := range spec {
		if !code[k] {
			t.Errorf("api/app.openapi.yaml documents %s, which the router does not serve", k)
		}
	}
}
