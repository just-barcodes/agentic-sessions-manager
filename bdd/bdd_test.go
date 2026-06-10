package bdd

import (
	"flag"
	"testing"

	"github.com/cucumber/godog"
)

// godogFormat picks the godog output format: pretty for authoring, progress
// (one dot per step, failures in full) for `task bdd`. Registered on the
// standard flag set so TestMain's flag.Parse picks it up.
var godogFormat = flag.String("godog.format", "pretty", "godog output format (pretty|progress|...)")

// TestFeatures runs the Gherkin scenarios under features/ against the real sm
// binary. Strict mode makes undefined or pending steps fail the suite.
func TestFeatures(t *testing.T) {
	if testing.Short() {
		t.Skip("BDD suite skipped in -short mode")
	}
	suite := godog.TestSuite{
		ScenarioInitializer: initializeScenario,
		Options: &godog.Options{
			Format:   *godogFormat,
			Paths:    []string{"features"},
			Strict:   true,
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("BDD scenarios failed")
	}
}
