package test

import (
	"flag"
	"strings"
	"testing"
)

func requireExplicitLifecycleTest(t *testing.T, testName string) {
	t.Helper()

	runPattern := ""
	if testRunFlag := flag.Lookup("test.run"); testRunFlag != nil {
		runPattern = strings.TrimSpace(testRunFlag.Value.String())
	}

	normalizedRunPattern := strings.TrimSuffix(strings.TrimPrefix(runPattern, "^"), "$")
	if runPattern == testName || normalizedRunPattern == testName {
		return
	}

	t.Skipf("%s uses live infrastructure; run it explicitly with -run %s", testName, testName)
}
