package scripts

import (
	"os"
	"strings"
	"testing"
)

func TestPackagedConfigRequiresSeparateTokenFile(t *testing.T) {
	body, err := os.ReadFile("../config/conf.yaml")
	if err != nil {
		t.Fatalf("read packaged config: %v", err)
	}
	config := string(body)
	if strings.Contains(config, "CHANGE_ME") {
		t.Fatal("packaged config contains the legacy public bearer token")
	}
	if !strings.Contains(config, `token_file: /etc/filegate/token`) || strings.Contains(config, "bearer_token:") {
		t.Fatal("packaged config must use a separately provisioned token file")
	}
}
