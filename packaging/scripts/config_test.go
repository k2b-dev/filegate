package scripts

import (
	"os"
	"strings"
	"testing"
)

func TestPackagedConfigUsesGeneratedBearerTokenBootstrap(t *testing.T) {
	body, err := os.ReadFile("../config/conf.yaml")
	if err != nil {
		t.Fatalf("read packaged config: %v", err)
	}
	config := string(body)
	if strings.Contains(config, "CHANGE_ME") {
		t.Fatal("packaged config contains the legacy public bearer token")
	}
	if !strings.Contains(config, `bearer_token: ""`) {
		t.Fatal("packaged config must leave auth.bearer_token empty for secure first-boot generation")
	}
}
