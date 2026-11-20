package secrets_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/torturado/agentcfg/internal/secrets"
)

func TestEveryRecipientDecryptsAndShellsReadTheEnvironmentFile(t *testing.T) {
	dir := t.TempDir()
	casa, portatil := filepath.Join(dir, "casa.txt"), filepath.Join(dir, "portatil.txt")
	var recipients []string
	for _, identity := range []string{casa, portatil} {
		recipient, err := secrets.Keygen(identity)
		if err != nil {
			t.Fatal(err)
		}
		recipients = append(recipients, recipient)
	}
	if _, err := secrets.Keygen(casa); err == nil {
		t.Error("keygen replaced an existing identity")
	}

	secretsPath := filepath.Join(dir, "secrets.age")
	want := secrets.Values{"DEPLOY_TOKEN": "tok_a;b&c|d>e#f", "SEARCH_API_KEY": "key-123"}
	if err := secrets.Save(secretsPath, want, recipients); err != nil {
		t.Fatal(err)
	}
	encrypted, _ := os.ReadFile(secretsPath)
	if string(encrypted) == string(want.Format()) {
		t.Fatal("secrets written in plain text")
	}

	for _, identity := range []string{casa, portatil} {
		got, err := secrets.Load(secretsPath, identity)
		if err != nil {
			t.Fatal(err)
		}
		envPath := filepath.Join(dir, filepath.Base(identity)+".env")
		if _, err := secrets.Sync(got, envPath); err != nil {
			t.Fatal(err)
		}
		if info, _ := os.Stat(envPath); info.Mode().Perm() != 0o600 {
			t.Errorf("environment file mode %v", info.Mode().Perm())
		}
		out, err := exec.Command("sh", "-c", `set -a; . "$0"; printf '%s %s' "$DEPLOY_TOKEN" "$SEARCH_API_KEY"`, envPath).Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != want["DEPLOY_TOKEN"]+" "+want["SEARCH_API_KEY"] {
			t.Errorf("shell read %q", out)
		}
	}

	outsider := filepath.Join(dir, "outsider.txt")
	if _, err := secrets.Keygen(outsider); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Load(secretsPath, outsider); err == nil {
		t.Error("a machine that is not a recipient decrypted the secrets")
	}
}

func TestValuesThatShellsAndSystemdWouldReadDifferentlyAreRejected(t *testing.T) {
	for _, value := range []string{"", "a b", "a'b", `a"b`, "a$b", `a\b`, "a`b", "a\nb"} {
		if err := secrets.ValidateValue("NAME", value); err == nil {
			t.Errorf("%q accepted", value)
		}
	}
}
