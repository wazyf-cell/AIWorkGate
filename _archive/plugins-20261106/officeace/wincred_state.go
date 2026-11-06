package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf16"
)

// OfficeAce 2026-10 builds store provider state outside storage.sqlite and keep
// only a wincred:// reference in the session row. Anything else is reported
// rather than guessed.
const winCredScheme = "wincred://"

type winCredEnvelope struct {
	SchemaVersion int    `json:"schemaVersion"`
	Encoding      string `json:"encoding"`
	StorageID     string `json:"storageId"`
	ChunkCount    int    `json:"chunkCount"`
}

// resolveProviderState follows a providerStateRef when the inline state is
// empty. Inline state always wins so older databases keep working.
func (rec *sessionRecord) resolveProviderState() {
	ref := strings.TrimSpace(rec.ProviderStateRef)
	if ref == "" || providerStatePresent(&rec.ProviderState) {
		return
	}
	target, ok := strings.CutPrefix(ref, winCredScheme)
	if !ok {
		rec.ProviderStateError = "OfficeAce 会话状态使用了未知的存储方式（" + ref + "），请更新 OfficeAce 插件"
		return
	}
	state, err := loadWinCredProviderState(strings.TrimSpace(target))
	if err != nil {
		rec.ProviderStateError = "OfficeAce 会话状态存放在 Windows 凭据管理器，读取失败：" + err.Error()
		return
	}
	rec.ProviderState = state
}

func providerStatePresent(ps *providerState) bool {
	if ps == nil {
		return false
	}
	return ps.AccountID != "" || ps.UserID != "" ||
		ps.ModelInfo.ModelAuthInfo.ModelAppKey != "" ||
		ps.Credential.STSToken != "" || ps.CredentialV3.STSToken != ""
}

// loadWinCredProviderState reassembles the chunked vault entry. The entry
// itself is a small JSON envelope; each chunk is base64 inside a UTF-16 blob.
func loadWinCredProviderState(target string) (providerState, error) {
	if target == "" {
		return providerState{}, fmt.Errorf("凭据名称为空")
	}
	blob, err := readWinCredBlob(target)
	if err != nil {
		return providerState{}, err
	}
	for _, text := range credentialTexts(blob) {
		var env winCredEnvelope
		if json.Unmarshal([]byte(text), &env) != nil || env.ChunkCount <= 0 {
			continue
		}
		var payload []byte
		payload, err = readWinCredChunks(target, env)
		if err != nil {
			return providerState{}, err
		}
		return parseProviderState(payload)
	}
	// Older envelopes hold the provider state document directly.
	var lastErr error
	for _, text := range credentialTexts(blob) {
		state, perr := parseProviderState([]byte(text))
		if perr == nil {
			return state, nil
		}
		lastErr = perr
	}
	return providerState{}, lastErr
}

func readWinCredChunks(target string, env winCredEnvelope) ([]byte, error) {
	if env.ChunkCount < 0 || env.ChunkCount > 512 {
		return nil, fmt.Errorf("凭据分片数量异常: %d", env.ChunkCount)
	}
	if strings.TrimSpace(env.StorageID) == "" {
		return nil, fmt.Errorf("凭据分片缺少 storageId")
	}
	if enc := strings.ToLower(strings.TrimSpace(env.Encoding)); enc != "" && enc != "base64" {
		return nil, fmt.Errorf("不支持的凭据编码: %s", env.Encoding)
	}
	var buf []byte
	for i := 0; i < env.ChunkCount; i++ {
		blob, err := readWinCredBlob(fmt.Sprintf("%s/chunks/%s/%d", target, env.StorageID, i))
		if err != nil {
			return nil, fmt.Errorf("读取凭据分片 %d/%d 失败: %w", i+1, env.ChunkCount, err)
		}
		chunk, err := decodeBase64Chunk(blob)
		if err != nil {
			return nil, fmt.Errorf("解码凭据分片 %d/%d 失败: %w", i+1, env.ChunkCount, err)
		}
		if len(buf)+len(chunk) > 4<<20 {
			return nil, fmt.Errorf("凭据内容超过 4 MiB 上限")
		}
		buf = append(buf, chunk...)
	}
	return buf, nil
}

func parseProviderState(payload []byte) (providerState, error) {
	var doc struct {
		ProviderState providerState `json:"providerState"`
	}
	if err := json.Unmarshal(payload, &doc); err == nil && providerStatePresent(&doc.ProviderState) {
		return doc.ProviderState, nil
	}
	var direct providerState
	if err := json.Unmarshal(payload, &direct); err == nil && providerStatePresent(&direct) {
		return direct, nil
	}
	return providerState{}, fmt.Errorf("凭据内容里没有 providerState")
}

// credentialTexts returns the UTF-16LE reading first, then the raw bytes, so a
// vault entry written either way still parses.
func credentialTexts(blob []byte) []string {
	out := make([]string, 0, 2)
	if s, ok := decodeUTF16LE(blob); ok {
		out = append(out, s)
	}
	out = append(out, string(blob))
	return out
}

func decodeUTF16LE(b []byte) (string, bool) {
	if len(b) < 2 || len(b)%2 != 0 {
		return "", false
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return strings.TrimPrefix(string(utf16.Decode(units)), "\ufeff"), true
}

func decodeBase64Chunk(blob []byte) ([]byte, error) {
	var lastErr error
	for _, text := range credentialTexts(blob) {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if out, err := base64.StdEncoding.DecodeString(text); err == nil {
			return out, nil
		}
		if out, err := base64.RawStdEncoding.DecodeString(text); err == nil {
			return out, nil
		}
		lastErr = fmt.Errorf("不是合法的 base64")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("分片为空")
	}
	return nil, lastErr
}
