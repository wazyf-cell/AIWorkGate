package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type localModel struct {
	ID                   string         `json:"id"`
	Model                string         `json:"model"`
	Name                 string         `json:"name"`
	DisplayName          string         `json:"displayName"`
	Provider             string         `json:"provider"`
	Kind                 string         `json:"kind"`
	Protocol             string         `json:"protocol"`
	ServiceType          string         `json:"serviceType"`
	BaseURL              string         `json:"baseUrl"`
	Enabled              *bool          `json:"enabled"`
	ToolCallingSupported *bool          `json:"toolCallingSupported"`
	ContextWindow        *float64       `json:"contextWindow"`
	ContextLength        *float64       `json:"context_length"`
	ContextLengthAlt     *float64       `json:"contextLength"`
	Capabilities         map[string]any `json:"capabilities"`
	InputModalities      []string       `json:"inputModalities"`
	ThinkingConfig       map[string]any `json:"thinking_config"`
	Efforts              any            `json:"efforts"`
	SupportedEfforts     any            `json:"supportedEfforts"`
	ReasoningEfforts     any            `json:"reasoningEfforts"`
	PriceFactor          *float64       `json:"priceFactor"`
	CostCoefficient      *float64       `json:"costCoefficient"`
	IsFree               *bool          `json:"isFree"`
	IsReasoning          *bool          `json:"isReasoning"`
	ModelSeries          string         `json:"modelSeries"`
	MaxOutputTokens      *float64       `json:"maxOutputTokens"`
}

type listModelsResponse struct {
	Success bool         `json:"success"`
	List    []localModel `json:"list"`
	Models  []localModel `json:"models"`
}

func (l *localClient) listModels(ctx context.Context, sessionID string) ([]localModel, error) {
	raw, _, err := l.do(ctx, http.MethodGet, "/api/maas-models", sessionID, nil)
	if err != nil {
		return nil, err
	}
	obj, err := unwrapObject(raw)
	if err != nil {
		return nil, fmt.Errorf("解析模型目录失败：%w", err)
	}
	b, _ := json.Marshal(obj)
	var resp listModelsResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("解析模型目录失败：%w", err)
	}
	if len(resp.List) > 0 {
		return resp.List, nil
	}
	return resp.Models, nil
}

type localProvider struct {
	ID               string            `json:"id"`
	DisplayName      string            `json:"displayName"`
	Protocol         string            `json:"protocol"`
	ServiceType      string            `json:"serviceType"`
	Models           []string          `json:"models"`
	ModelBaseURLs    map[string]string `json:"modelBaseUrls"`
	ModelDisplayName map[string]string `json:"modelDisplayNames"`
	Enabled          *bool             `json:"enabled"`
}

type localProfiles struct {
	MaasAPIKey string          `json:"maasApiKey"`
	Providers  []localProvider `json:"providers"`
}

type modelTokenRate struct {
	ModelName       string   `json:"modelName"`
	CostCoefficient *float64 `json:"costCoefficient"`
}

type modelTokenRates struct {
	Models []modelTokenRate `json:"models"`
}

// getTokenRates reads the per-model credit multiplier OfficeAce shows as "Nx".
// Models missing from the response have no published rate.
func (l *localClient) getTokenRates(ctx context.Context, sessionID string) (map[string]float64, error) {
	raw, _, err := l.do(ctx, http.MethodGet, "/api/subscription/model-token-rates", sessionID, nil)
	if err != nil {
		return nil, err
	}
	var resp modelTokenRates
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析模型倍率失败：%w", err)
	}
	out := make(map[string]float64, len(resp.Models))
	for _, rate := range resp.Models {
		id := strings.TrimSpace(rate.ModelName)
		if id == "" || rate.CostCoefficient == nil {
			continue
		}
		out[id] = *rate.CostCoefficient
	}
	return out, nil
}

func (l *localClient) getProfiles(ctx context.Context, sessionID string) (*localProfiles, error) {
	raw, _, err := l.do(ctx, http.MethodGet, "/api/model-config-profiles", sessionID, nil)
	if err != nil {
		return nil, err
	}
	obj, err := unwrapObject(raw)
	if err != nil {
		return nil, fmt.Errorf("解析模型档案失败：%w", err)
	}
	b, _ := json.Marshal(obj)
	var p localProfiles
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("解析模型档案字段失败：%w", err)
	}
	return &p, nil
}
