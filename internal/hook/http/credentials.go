package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cslab-ntua/HPA-Plus/internal/hook"
	validation "k8s.io/apimachinery/pkg/util/validation"
)

var ErrCredentialConfiguration = errors.New("invalid hook credential binding configuration")
var ErrCredentialsUnavailable = errors.New("hook credentials unavailable or unauthorized")

const maxTokenBytes = 8192
const maxBindingBytes = 1024 * 1024

// CredentialBinding contains only public routing information, never token data.
// Secret files live at <directory>/tokens/<name> and are read for every request.
type CredentialBinding struct {
	Name          string   `json:"name"`
	Origin        string   `json:"origin"`
	Namespace     string   `json:"namespace"`
	ResourceNames []string `json:"resourceNames"`
}
type credentialKey struct{ origin, namespace, name string }
type credentials struct {
	directory string
	bindings  map[credentialKey]string
}

// NewWithCredentials adds opt-in administrator-owned credentials. Binding changes
// require restart; projected Secret token rotation does not. No Secret API access.
func NewWithCredentials(origins []string, directory string) (*Execute, error) {
	e, err := New(origins)
	if err != nil {
		return nil, err
	}
	if directory == "" {
		return e, nil
	}
	raw, err := readBoundedFile(filepath.Join(directory, "bindings.json"), maxBindingBytes)
	if err != nil {
		return nil, ErrCredentialConfiguration
	}
	var bindings []CredentialBinding
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bindings); err != nil {
		return nil, ErrCredentialConfiguration
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF || len(bindings) == 0 || len(bindings) > 64 {
		return nil, ErrCredentialConfiguration
	}
	c := &credentials{directory: directory, bindings: map[credentialKey]string{}}
	names := map[string]bool{}
	for _, binding := range bindings {
		if len(validation.IsDNS1123Label(binding.Name)) != 0 || names[binding.Name] || len(validation.IsDNS1123Label(binding.Namespace)) != 0 || len(binding.ResourceNames) == 0 || len(binding.ResourceNames) > 64 {
			return nil, ErrCredentialConfiguration
		}
		names[binding.Name] = true
		u, err := url.Parse(binding.Origin)
		if err != nil {
			return nil, ErrCredentialConfiguration
		}
		key, err := origin(u)
		if err != nil || u.Scheme != "https" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery {
			return nil, ErrCredentialConfiguration
		}
		if _, ok := e.origins[key]; !ok {
			return nil, ErrCredentialConfiguration
		}
		for _, name := range binding.ResourceNames {
			if len(validation.IsDNS1123Subdomain(name)) != 0 {
				return nil, ErrCredentialConfiguration
			}
			scope := credentialKey{key, binding.Namespace, name}
			if _, ok := c.bindings[scope]; ok {
				return nil, ErrCredentialConfiguration
			}
			c.bindings[scope] = binding.Name
		}
	}
	e.credentials = c
	return e, nil
}

func readBoundedFile(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrCredentialsUnavailable
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit+1)))
	if err != nil || len(b) > limit {
		return nil, ErrCredentialsUnavailable
	}
	return b, nil
}

var bearerToken = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

func (e *Execute) authenticate(ctx context.Context, req *http.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.credentials == nil {
		return nil
	}
	resource := hook.ResourceFromContext(ctx)
	if resource.Namespace == "" || resource.Name == "" {
		return ErrCredentialsUnavailable
	}
	key, err := origin(req.URL)
	if err != nil {
		return ErrCredentialsUnavailable
	}
	binding, ok := e.credentials.bindings[credentialKey{key, resource.Namespace, resource.Name}]
	if !ok {
		return nil
	} // This scope has no operator-managed credentials.
	for name := range req.Header {
		if strings.EqualFold(name, "Authorization") {
			return ErrCredentialsUnavailable
		}
	}
	raw, err := readBoundedFile(filepath.Join(e.credentials.directory, "tokens", binding), maxTokenBytes)
	if err != nil {
		return ErrCredentialsUnavailable
	}
	token := strings.TrimSpace(string(raw))
	if !bearerToken.MatchString(token) {
		return ErrCredentialsUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}
