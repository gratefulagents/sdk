package agentsdk

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSDKDoesNotDependOnOperatorPackages(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list SDK deps: %v\n%s", err, out)
	}

	forbidden := []string{
		"github.com/gratefulagents/sdk/api/",
		"github.com/gratefulagents/sdk/internal/controller/",
		"github.com/gratefulagents/sdk/internal/agentplatform",
		"github.com/gratefulagents/sdk/internal/anthropic",
		"github.com/gratefulagents/sdk/internal/openai",
		"github.com/gratefulagents/sdk/internal/store/",
		"github.com/gratefulagents/sdk/internal/tools",
		"github.com/aws/",
		"github.com/anthropics/",
		"github.com/openai/",
		"go.opentelemetry.io/",
		"k8s.io/",
		"sigs.k8s.io/",
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, prefix := range forbidden {
			if strings.HasPrefix(dep, prefix) {
				t.Fatalf("SDK dependency %q crosses forbidden operator boundary %q", dep, prefix)
			}
		}
	}
}
