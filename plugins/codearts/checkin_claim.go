package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func (p *Plugin) fetchStatistics(ctx context.Context, acct *Account, proxyURL string) (*packageInfo, error) {
	res := p.snapRequest(ctx, http.MethodGet, p.endpoints.Statistics, nil, acct, proxyURL)
	if !res.ok {
		return nil, snapError("账户信息查询失败", res)
	}
	info := &packageInfo{}
	if pkg, ok := res.data["package"].(map[string]any); ok {
		info.IsCreditPackage = boolValue(pkg["is_credit_package"])
		info.IsTokenPackage = boolValue(pkg["is_token_package"])
		info.PackageName = stringValue(pkg["package_name_cn"])
		if info.PackageName == "" {
			info.PackageName = stringValue(pkg["package_name_en"])
		}
	}
	metrics, _ := res.data["metrics"].([]any)
	remaining, total, hasCredit := parseCreditMetrics(metrics)
	info.Remaining, info.Total, info.HasCredit = remaining, total, hasCredit
	return info, nil
}

func parseCreditMetrics(metrics []any) (remaining, total float64, ok bool) {
	sawAny, sawTotal := false, false
	var totalRemain, totalAmount float64
	var sumRemain, sumAmount float64
	for _, item := range metrics {
		rec, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		name := stringValue(rec["name"])
		if !creditMetricLabels[name] {
			continue
		}
		sawAny = true
		amount, _ := numberValue(rec["package_credit_amount"])
		remain, _ := numberValue(rec["package_credit_remain"])
		if name == totalCreditMetric {
			// Aggregate bucket: it mirrors the sum of the per-package metrics,
			// so counting its amount again would double the total.
			sawTotal, totalRemain, totalAmount = true, remain, amount
			continue
		}
		if amount <= 0 && remain <= 0 {
			continue
		}
		sumRemain += remain
		sumAmount += amount
	}
	if !sawAny {
		return 0, 0, false
	}
	if sawTotal {
		remaining = totalRemain
	} else {
		remaining = sumRemain
	}
	switch {
	case sumAmount > 0:
		total = sumAmount
	case totalAmount > 0:
		total = totalAmount
	case sawTotal:
		total = totalRemain
	default:
		total = remaining
	}
	return roundCredits(remaining), roundCredits(total), true
}

func roundCredits(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func (p *Plugin) fetchActivities(ctx context.Context, acct *Account, proxyURL string) ([]opsActivity, error) {
	rawURL := deliveryURL(p.endpoints.Delivery)
	res := p.snapRequest(ctx, http.MethodGet, rawURL, nil, acct, proxyURL)
	if !res.ok {
		return nil, snapError("活动列表查询失败", res)
	}
	items, ok := res.data["items"].([]any)
	if !ok {
		return nil, fmt.Errorf("活动列表查询失败：响应缺少 items 字段")
	}
	out := make([]opsActivity, 0, len(items))
	for _, item := range items {
		rec, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		campaign, _ := identifierValue(rec["campaignId"])
		amount, _ := numberValue(rec["benefitAmount"])
		if amount == 0 {
			amount, _ = numberValue(rec["amount"])
		}
		if amount == 0 {
			amount, _ = numberValue(rec["creditAmount"])
		}
		out = append(out, opsActivity{
			CampaignID: campaign,
			Type:       stringValue(rec["type"]),
			Title:      stringValue(rec["title"]),
			Claimable:  boolValue(rec["claimable"]),
			Status:     stringValue(rec["status"]),
			Amount:     amount,
		})
	}
	return out, nil
}

var claimedStatuses = map[string]bool{"CLAIMED": true, "CONFIRMED": true, "CONSUMED": true}

// claimDailyCheckin implements the reference flow: statistics -> activities ->
// claim -> confirm. There is no status endpoint; a successful claim is the
// only positive signal.
func (p *Plugin) claimDailyCheckin(ctx context.Context, acct *Account, proxyURL string) (checkinResult, error) {
	now := p.now()
	res := checkinResult{At: now.Unix()}
	info, err := p.fetchStatistics(ctx, acct, proxyURL)
	if err != nil {
		return res, err
	}
	before := info.Remaining
	if !info.IsCreditPackage {
		if info.IsTokenPackage {
			return res, fmt.Errorf("Token 计费账户，不在积分活动范围")
		}
		return res, fmt.Errorf("非积分计费账户，不在积分活动范围")
	}
	acts, err := p.fetchActivities(ctx, acct, proxyURL)
	if err != nil {
		return res, err
	}
	var activity *opsActivity
	for i := range acts {
		if acts[i].Type == "USER_LOGIN" {
			activity = &acts[i]
			break
		}
	}
	if activity == nil {
		return res, fmt.Errorf("未找到每日签到活动")
	}
	if !activity.Claimable {
		res.Result = "already"
		if info.HasCredit {
			res.CreditsBefore = floatPtr(before)
			res.CreditsAfter = floatPtr(before)
		}
		if claimedStatuses[activity.Status] {
			res.Message = "今天已领取"
		} else {
			res.Message = fmt.Sprintf("当前不可领取（status=%s）", activity.Status)
		}
		return res, nil
	}
	if activity.CampaignID == nil {
		return res, fmt.Errorf("活动缺少 campaignId，无法领取")
	}
	body, _ := json.Marshal(map[string]any{"campaignId": activity.CampaignID, "channel": "IDE"})
	claim := p.snapRequest(ctx, http.MethodPost, p.endpoints.Claim, body, acct, proxyURL)
	if !claim.ok {
		return res, snapError("", claim)
	}
	if claim.data["id"] != nil {
		confirmBody, _ := json.Marshal(map[string]any{"campaignId": activity.CampaignID})
		_ = p.snapRequest(ctx, http.MethodPost, p.endpoints.Confirm, confirmBody, acct, proxyURL)
	}
	credit := 0.0
	for _, key := range []string{"benefitAmount", "credit", "credits", "creditAmount", "amount"} {
		if v, ok := numberValue(claim.data[key]); ok && v > 0 {
			credit = v
			break
		}
	}
	if credit == 0 {
		credit = activity.Amount
	}
	res.Result = "ok"
	if credit > 0 && info.HasCredit {
		res.CreditsBefore = floatPtr(before)
		res.CreditsAfter = floatPtr(roundCredits(before + credit))
		res.Message = fmt.Sprintf("+%g", credit)
	} else if credit > 0 {
		res.Message = fmt.Sprintf("+%g", credit)
	} else {
		res.Message = "签到成功"
	}
	return res, nil
}

func deliveryURL(base string) string {
	if strings.Contains(base, "?") {
		return base + "&channel=IDE"
	}
	return base + "?channel=IDE"
}
