package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type bonusSKU struct {
	ActivityID   string  `json:"activityId"`
	ActivityName string  `json:"activityName"`
	Points       float64 `json:"points"`
	CurrentValue float64 `json:"currentValue"`
	Effective    string  `json:"effectiveTime"`
	Expired      string  `json:"expiredTime"`
}

type creditGroup struct {
	Key       string  `json:"key"`
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	ExpiresAt string  `json:"expiresAt"`
	Expired   bool    `json:"expired"`
}

type subscription struct {
	PlanID           string        `json:"planId"`
	PlanName         string        `json:"planName"`
	Tier             string        `json:"tier"`
	Status           string        `json:"status"`
	ExpiresAt        string        `json:"expiresAt"`
	UsedCredits      float64       `json:"usedCredits"`
	TotalCredits     float64       `json:"totalCredits"`
	UnlimitedCredits bool          `json:"unlimitedCredits"`
	BonusSKUs        []bonusSKU    `json:"bonusSkus"`
	CreditGroups     []creditGroup `json:"creditGroups"`
}

func (s *subscription) remaining() (float64, bool) {
	if s == nil {
		return 0, false
	}
	if s.UnlimitedCredits || s.TotalCredits < 0 {
		return -1, true
	}
	rem := s.TotalCredits - s.UsedCredits
	if rem < 0 {
		rem = 0
	}
	return rem, true
}

func (l *localClient) getIsLogin(ctx context.Context, sessionID string) (bool, string, error) {
	raw, _, err := l.do(ctx, http.MethodGet, "/api/islogin", sessionID, nil)
	if err != nil {
		return false, "", err
	}
	var payload struct {
		IsLogin bool   `json:"islogin"`
		UserID  string `json:"userId"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, "", fmt.Errorf("解析 /api/islogin 失败：%w", err)
	}
	return payload.IsLogin, payload.UserID, nil
}

func (l *localClient) getSubscription(ctx context.Context, sessionID string) (*subscription, error) {
	raw, _, err := l.do(ctx, http.MethodGet, "/api/subscription/current", sessionID, nil)
	if err != nil {
		return nil, err
	}
	obj, err := unwrapObject(raw)
	if err != nil {
		return nil, fmt.Errorf("解析订阅信息失败：%w", err)
	}
	normalized, _ := json.Marshal(obj)
	var sub subscription
	if err := json.Unmarshal(normalized, &sub); err != nil {
		return nil, fmt.Errorf("解析订阅字段失败：%w", err)
	}
	return &sub, nil
}

// bonusClaim is the result of POST /api/subscription/claim-bonus.
type bonusClaim struct {
	SKUs  []bonusSKU
	Body  []byte
	Claim float64
}

func (l *localClient) claimBonus(ctx context.Context, sessionID string) (*bonusClaim, error) {
	raw, _, err := l.do(ctx, http.MethodPost, "/api/subscription/claim-bonus", sessionID, []byte("{}"))
	if err != nil {
		return nil, err
	}
	obj, err := unwrapObject(raw)
	if err != nil {
		return nil, fmt.Errorf("解析领取结果失败：%w", err)
	}
	var payload struct {
		BonusSKUs []bonusSKU `json:"bonusSkus"`
		Bonus     []bonusSKU `json:"bonus_skus"`
		SKUs      []bonusSKU `json:"skus"`
		Error     string     `json:"error"`
	}
	b, _ := json.Marshal(obj)
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, fmt.Errorf("解析领取字段失败：%w", err)
	}
	if payload.Error != "" {
		return nil, fmt.Errorf("领取接口返回错误：%s", payload.Error)
	}
	skus := payload.BonusSKUs
	if len(skus) == 0 {
		skus = payload.Bonus
	}
	if len(skus) == 0 {
		skus = payload.SKUs
	}
	claim := &bonusClaim{SKUs: skus, Body: raw}
	for _, s := range skus {
		if d := s.Points - s.CurrentValue; d > 0 {
			claim.Claim += d
		}
	}
	return claim, nil
}
