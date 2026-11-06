// Package main — local_state login for ZCode.
//
// The official client persists its login in %USERPROFILE%\.zcode\v2\credentials.json.
// Every value is an `enc:v1:<iv>.<tag>.<ciphertext>` envelope sealed with
// AES-256-GCM, keyed by sha256(secret) where secret is ZCODE_CREDENTIAL_SECRET
// or `zcode-credential-fallback:<platform>:<homedir>:<username>`.
//
// The scheme was read out of the installed client (resources/app.asar,
// createCredentialCipherProvider) and is reproduced here field for field; the
// reference JS adapter (docs/_ref/def-zcode.js) documents that it imports the
// same file. Nothing is invented: an unreadable file is an error, never a login.
package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	envelopePrefix = "enc:v1:"
	credEnvName    = "ZCODE_CREDENTIAL_SECRET"
	// credentialsFileName is the official client's store.
	credentialsFileName = "credentials.json"
	// credentialKindAPIKey is the `id.secret` coding-plan key (api.z.ai).
	credentialKindAPIKey = "api_key"
	// credentialKindJWT is the ZCode desktop JWT (zcode.z.ai zcode-plan).
	credentialKindJWT = "jwt"
)

// localImport is the outcome of a successful local_state read.
type localImport struct {
	Kind   string
	Secret string
	Label  string
	// Source is a non-secret description of where the credential came from.
	Source string
}

// localStateError carries an operator-readable reason plus the HTTP status the
// handler should answer with. Credential values never enter it.
type localStateError struct {
	Status  int
	Code    string
	Message string
}

func (e *localStateError) Error() string { return e.Message }

func localStateFile() string {
	if override := strings.TrimSpace(os.Getenv("ZCODE_CREDENTIALS_FILE")); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".zcode", "v2", credentialsFileName)
}

// credentialSecret mirrors defaultCredentialSecret in the official client.
func credentialSecret() string {
	if v := os.Getenv(credEnvName); v != "" {
		return v
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32" // node's os.platform()
	}
	home, _ := os.UserHomeDir()
	return "zcode-credential-fallback:" + platform + ":" + home + ":" + os.Getenv("USERNAME")
}

// decryptCredential opens one enc:v1 envelope. A plain value is returned as is:
// the client tolerates that shape for old files.
func decryptCredential(value, secret string) (string, error) {
	if !strings.HasPrefix(value, envelopePrefix) {
		return value, nil
	}
	parts := strings.Split(strings.TrimPrefix(value, envelopePrefix), ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("密文格式非法")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("iv 非法")
	}
	tag, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("auth tag 非法")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("ciphertext 非法")
	}
	sum := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(nonce) != gcm.NonceSize() || len(tag) != gcm.Overhead() {
		return "", fmt.Errorf("密文长度非法")
	}
	plain, err := gcm.Open(nil, nonce, append(ciphertext, tag...), nil)
	if err != nil {
		return "", fmt.Errorf("密钥不匹配或密文已损坏")
	}
	return string(plain), nil
}

// readLocalCredentials loads and decrypts the credential map. The returned map
// is only ever used in memory.
func readLocalCredentials() (map[string]string, error) {
	path := localStateFile()
	if path == "" {
		return nil, &localStateError{Status: 400, Code: "local_state_missing",
			Message: "找不到用户主目录，无法定位 ZCode 凭据文件"}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &localStateError{Status: 400, Code: "local_state_missing",
				Message: "未找到 " + path + "：请先在 ZCode 官方客户端登录一次"}
		}
		return nil, &localStateError{Status: 400, Code: "local_state_unreadable",
			Message: "读取 " + path + " 失败: " + err.Error()}
	}
	var stored map[string]string
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, &localStateError{Status: 400, Code: "local_state_corrupt",
			Message: path + " 不是预期的 JSON 对象: " + err.Error()}
	}
	secret := credentialSecret()
	out := make(map[string]string, len(stored))
	for key, value := range stored {
		plain, derr := decryptCredential(value, secret)
		if derr != nil {
			return nil, &localStateError{Status: 400, Code: "local_state_unreadable",
				Message: "解密 " + key + " 失败（" + derr.Error() + "）：若客户端设置过 ZCODE_CREDENTIAL_SECRET，请把它注入插件进程"}
		}
		out[key] = plain
	}
	if len(out) == 0 {
		return nil, &localStateError{Status: 400, Code: "local_state_empty",
			Message: path + " 已解密但没有任何条目"}
	}
	return out, nil
}

// pickLocalCredential chooses the credential that can actually drive a request.
//
// Preference order is deliberate: the coding-plan api key answers on api.z.ai
// without the Aliyun captcha the zcode-plan channel demands, so it is the only
// one that can serve chat unattended. The desktop JWT is kept as a fallback
// because it still validates against the billing endpoint.
func pickLocalCredential(env map[string]string) (localImport, error) {
	for key, value := range env {
		if !strings.HasPrefix(key, "account-provider:coding-plan:account:") || !strings.HasSuffix(key, ":api-key") {
			continue
		}
		if strings.TrimSpace(value) == "" {
			continue
		}
		label := userDisplayName(env)
		if label == "" {
			if name, ok := providerLabel(env, key); ok {
				label = name
			}
		}
		if label == "" {
			label = "ZCode"
		}
		return localImport{Kind: credentialKindAPIKey, Secret: value, Label: label,
			Source: key}, nil
	}
	if jwt := strings.TrimSpace(env["zcodejwttoken"]); jwt != "" {
		return localImport{Kind: credentialKindJWT, Secret: jwt,
			Label: firstNonEmpty(userDisplayName(env), "ZCode"), Source: "zcodejwttoken"}, nil
	}
	return localImport{}, &localStateError{Status: 400, Code: "local_state_no_credential",
		Message: "凭据文件里既没有 coding-plan api-key 也没有 zcodejwttoken"}
}

// providerLabel derives "bigmodel-individual-coding-plan" (or the team plan)
// from the account-provider key so the account label is not a bare "ZCode".
func providerLabel(env map[string]string, key string) (string, bool) {
	const prefix = "account-provider:coding-plan:account:"
	rest := strings.TrimPrefix(key, prefix)
	name, _, ok := strings.Cut(rest, ":account:")
	if !ok || strings.TrimSpace(name) == "" {
		return "", false
	}
	return name, true
}

// userDisplayName reads oauth:bigmodel:user_info without exposing the blob.
func userDisplayName(env map[string]string) string {
	raw := strings.TrimSpace(env["oauth:bigmodel:user_info"])
	if raw == "" {
		return ""
	}
	var info struct {
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
	}
	if json.Unmarshal([]byte(raw), &info) != nil {
		return ""
	}
	return firstNonEmpty(strings.TrimSpace(info.DisplayName), strings.TrimSpace(info.Username))
}

// importLocalState reads the client state and validates the credential with a
// real upstream call before anything is stored.
func (s *server) importLocalState(ctx context.Context) (localImport, error) {
	if s.importOverride != nil {
		return s.importOverride(ctx)
	}
	env, err := readLocalCredentials()
	if err != nil {
		return localImport{}, err
	}
	imp, err := pickLocalCredential(env)
	if err != nil {
		return localImport{}, err
	}
	if imp.Kind == credentialKindAPIKey {
		err = s.client.validateAPIKey(ctx, apiKeyOrigin, imp.Secret, "")
	} else {
		err = s.client.validateJWT(ctx, originBase, imp.Secret, "")
	}
	if err != nil {
		return localImport{}, &localStateError{Status: 400, Code: "local_state_rejected",
			Message: "上游拒绝了这个凭据（" + err.Error() + "）"}
	}
	return imp, nil
}
