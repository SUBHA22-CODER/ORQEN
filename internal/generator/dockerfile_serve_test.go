package generator

import (
	"strings"
	"testing"

	"github.com/SUBHA22-CODER/ORQEN/internal/domain"
)

func TestGenerateDockerfile_GoServerMode(t *testing.T) {
	svc := &domain.Service{
		Name:      "orqen",
		Language:  domain.LangGo,
		Framework: domain.FrameworkGo,
		Port:      8420,
		HostPort:  8420,
		StartCmd:  "./app serve --host 0.0.0.0 --port 8420 --no-open",
	}

	df := GenerateDockerfile(svc, Options{})
	if !strings.Contains(df, "EXPOSE 8420") {
		t.Errorf("expected EXPOSE 8420, got:\n%s", df)
	}
	if !strings.Contains(df, `CMD ["/app/app","serve","--host","0.0.0.0","--port","8420","--no-open"]`) {
		t.Errorf("expected exec form CMD with serve flags, got:\n%s", df)
	}
}

func TestGenerateDockerfile_GoPureCLI(t *testing.T) {
	svc := &domain.Service{
		Name:      "simplecli",
		Language:  domain.LangGo,
		Framework: domain.FrameworkGo,
		StartCmd:  "./app",
	}

	df := GenerateDockerfile(svc, Options{})
	if !strings.Contains(df, `CMD ["/app/app"]`) {
		t.Errorf("expected standard CMD [\"/app/app\"] for pure CLI, got:\n%s", df)
	}
}
