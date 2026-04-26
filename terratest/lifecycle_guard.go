package test

import (
	"flag"
	"regexp"
	"strings"
	"testing"
)

var explicitLifecycleTests = []string{
	"TestHAWriteLocalSuiteEnv",
	"TestHAOverrideLocalWebhook",
	"TestHAOverrideDownstreamWebhook",
	"TestHAWaitWebhookChartVersion",
	"TestHAWaitReady",
	"TestHAUpgradeRancher",
	"TestHaSetup",
	"TestHACleanup",
	"TestHAControlPanel",
	"TestHAProvisionLinodeDownstream",
	"TestHADeleteLinodeDownstream",
}

func requireExplicitLifecycleTest(t *testing.T, testName string) {
	t.Helper()

	runPattern := ""
	if testRunFlag := flag.Lookup("test.run"); testRunFlag != nil {
		runPattern = strings.TrimSpace(testRunFlag.Value.String())
	}

	if isExplicitLifecycleRun(runPattern, testName) {
		return
	}

	t.Skipf("%s uses live infrastructure; run it explicitly with -run %s", testName, testName)
}

func isExplicitLifecycleRun(runPattern, testName string) bool {
	runPattern = strings.TrimSpace(runPattern)
	if runPattern == "" {
		return false
	}

	topLevelPattern := strings.SplitN(runPattern, "/", 2)[0]
	if topLevelPattern == "" {
		return false
	}

	runRegex, err := regexp.Compile(topLevelPattern)
	if err != nil {
		return false
	}
	if !runRegex.MatchString(testName) {
		return false
	}

	matches := 0
	for _, lifecycleTest := range explicitLifecycleTests {
		if runRegex.MatchString(lifecycleTest) {
			matches++
		}
	}
	return matches == 1
}
