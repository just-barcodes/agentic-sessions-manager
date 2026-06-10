package bdd

import (
	"testing"

	"github.com/cucumber/godog"
)

// TestFeatures runs the Gherkin scenarios under features/ against the real sm
// binary. Strict mode makes undefined or pending steps fail the suite.
func TestFeatures(t *testing.T) {
	if testing.Short() {
		t.Skip("BDD suite skipped in -short mode")
	}
	suite := godog.TestSuite{
		ScenarioInitializer: initializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			Strict:   true,
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("BDD scenarios failed")
	}
}
