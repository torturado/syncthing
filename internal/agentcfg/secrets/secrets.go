// Package secrets keeps the credentials that agent configuration references as
// ${VAR}. They are stored in secrets.age, which is synced and encrypted to
// every machine's age public key, and decrypted on each machine into an
// unsynced environment file read by systemd user units and login shells.
package secrets

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"filippo.io/age"

	"github.com/torturado/agentcfg/internal/files"
)

var name = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// unsafe lists what a value may not contain. The environment file is read by
// both systemd's environment.d parser and POSIX shells, which agree on a
// single-quoted value only when it has no quote, no $ and no backslash.
const unsafe = "'\"$\\`"

// Values maps variable names to secret values.
type Values map[string]string

func ValidateName(n string) error {
	if !name.MatchString(n) {
		return fmt.Errorf("%q is not a valid variable name", n)
	}
	return nil
}

func ValidateValue(n, value string) error {
	if value == "" {
		return fmt.Errorf("%s: empty value", n)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == ' ' || strings.ContainsRune(unsafe, r) {
			return fmt.Errorf("%s: value contains whitespace, a control character or one of %s", n, unsafe)
		}
	}
	return nil
}

// Format renders values as an environment file, one NAME='value' per line.
func (v Values) Format() []byte {
	var buf bytes.Buffer
	buf.WriteString("# Written by agentcfg secrets sync. Do not edit or sync this file.\n")
	for _, key := range slices.Sorted(maps.Keys(v)) {
		fmt.Fprintf(&buf, "%s='%s'\n", key, v[key])
	}
	return buf.Bytes()
}

func Parse(data []byte) (Values, error) {
	values := Values{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected NAME='value'", line)
		}
		value = strings.TrimSuffix(strings.TrimPrefix(value, "'"), "'")
		if err := ValidateName(key); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if err := ValidateValue(key, value); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		values[key] = value
	}
	return values, scanner.Err()
}

// Load decrypts the secrets file with the identity at identityPath. A missing
// secrets file holds no values.
func Load(secretsPath, identityPath string) (Values, error) {
	encrypted, err := os.ReadFile(secretsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return Values{}, nil
	}
	if err != nil {
		return nil, err
	}
	identities, err := readIdentities(identityPath)
	if err != nil {
		return nil, err
	}
	reader, err := age.Decrypt(bytes.NewReader(encrypted), identities...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", secretsPath, err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", secretsPath, err)
	}
	return Parse(plain)
}

// Save encrypts values to every recipient and replaces the secrets file.
func Save(secretsPath string, values Values, recipients []string) error {
	if len(recipients) == 0 {
		return fmt.Errorf("no age_recipient in the spec's [hosts]; nobody could decrypt the secrets")
	}
	var parsed []age.Recipient
	for _, recipient := range recipients {
		r, err := age.ParseX25519Recipient(recipient)
		if err != nil {
			return fmt.Errorf("age_recipient %q: %w", recipient, err)
		}
		parsed = append(parsed, r)
	}
	var buf bytes.Buffer
	writer, err := age.Encrypt(&buf, parsed...)
	if err != nil {
		return err
	}
	if _, err := writer.Write(values.Format()); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return files.Atomic(secretsPath, buf.Bytes(), 0o600)
}

// Keygen creates a new identity at path and returns its public key. An
// existing identity is never replaced.
func Keygen(path string) (string, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	content := fmt.Sprintf("# public key: %s\n%s\n", identity.Recipient(), identity)
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		return "", err
	}
	return identity.Recipient().String(), file.Close()
}

// Sync writes the decrypted values to the environment file with mode 0600,
// and reports whether its content changed.
func Sync(values Values, envPath string) (bool, error) {
	data := values.Format()
	current, err := os.ReadFile(envPath)
	if err == nil && bytes.Equal(current, data) {
		return false, os.Chmod(envPath, 0o600)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(envPath), 0o700); err != nil {
		return false, err
	}
	return true, files.Atomic(envPath, data, 0o600)
}

func readIdentities(path string) ([]age.Identity, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("identity: %w (create one with: agentcfg secrets keygen)", err)
	}
	defer file.Close()
	identities, err := age.ParseIdentities(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return identities, nil
}
